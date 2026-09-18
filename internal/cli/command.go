package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"
)

type options struct {
	instance, container, db, access, host, admin, sslmode, passwordEnv, field, format string
	port                                                                              int
	json, quiet, force                                                                bool
}

func (o options) validate() error {
	if o.instance != "" && o.container != "" {
		return errors.New("choose --instance or --container, not both")
	}
	if o.access != "owner" {
		return errors.New("only --access owner is supported; ro/rw are deferred")
	}
	if o.port < 1 || o.port > 65535 {
		return errors.New("port must be 1–65535")
	}
	if !slices.Contains([]string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}, o.sslmode) {
		return errors.New("invalid sslmode")
	}
	if o.format != "" && o.format != "json" && o.format != "env" {
		return errors.New("format must be json or env")
	}
	if o.field != "" && o.field != "url" && o.field != "password" {
		return errors.New("field must be url or password")
	}
	if o.field != "" && o.format != "" {
		return errors.New("choose --field or --format")
	}
	if o.json && o.format == "env" {
		return errors.New("--json conflicts with --format env")
	}
	return nil
}

func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	cmd := newCommand(in, out, errOut)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

func newCommand(in io.Reader, out, errOut io.Writer) *cobra.Command {
	var o options
	root := &cobra.Command{
		Use: "pogg", Short: "Provision PostgreSQL databases and credentials for local development",
		Long:         "Provision PostgreSQL databases and credentials for local development.\n\nQuick start (creates an app, database, and owner login):\n  pogg create myapp\n  pogg url myapp\n\nUse app create only for an empty app with manually configured databases and users.\n\nState: $PGPLEASE_CONFIG_DIR or $XDG_CONFIG_HOME/pgplease (default ~/.config/pgplease).\nSync restores missing objects, NOT data. Unknown server objects are never imported or deleted.",
		SilenceUsage: true, SilenceErrors: true,
	}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	flags := root.PersistentFlags()
	flags.StringVar(&o.instance, "instance", "", "Choose a registered instance")
	flags.StringVar(&o.instance, "on", "", "Alias for --instance")
	flags.StringVar(&o.container, "container", "", "Discover a running local Docker container")
	flags.BoolVar(&o.json, "json", false, "Emit structured JSON output and errors")
	flags.BoolVar(&o.quiet, "quiet", false, "Suppress successful mutation status messages")

	// Open and lock state only for application commands, never help or completion.
	add := func(parent *cobra.Command, use, short string, args cobra.PositionalArgs, run func(context.Context, *runner, []string) error) *cobra.Command {
		cmd := &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			dir, err := configDir()
			if err != nil {
				return err
			}
			s, release, err := openStore(dir)
			if err != nil {
				return err
			}
			defer release()
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			r := &runner{s: s, o: o, in: cmd.InOrStdin(), out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr()}
			return run(ctx, r, args)
		}}
		parent.AddCommand(cmd)
		return cmd
	}
	list := func(_ context.Context, r *runner, _ []string) error { return r.output(r.s.state.Apps) }
	add(root, "list", "List apps without passwords", cobra.NoArgs, list)
	add(root, "create APP", "Create an app, database, and owner login", cobra.ExactArgs(1), func(ctx context.Context, r *runner, a []string) error { return r.createApp(ctx, a[0], true) })
	add(root, "info APP", "Show app metadata without passwords", cobra.ExactArgs(1), func(_ context.Context, r *runner, args []string) error {
		_, key, a, err := r.app(args[0])
		if err != nil {
			return err
		}
		if key != "" {
			return errors.New("expected an app name")
		}
		return r.output(a)
	})
	for _, name := range []string{"url", "get"} {
		short := "Print only the PostgreSQL connection URL"
		if name == "get" {
			short = "Retrieve credentials as JSON, shell assignments, or a field"
		}
		cmd := add(root, name+" APP[/USER]", short, cobra.ExactArgs(1), func(ctx context.Context, r *runner, a []string) error { return r.credentials(ctx, name, a[0]) })
		cmd.Flags().StringVar(&o.db, "db", "", "Select a database alias")
		if name == "get" {
			cmd.Flags().StringVar(&o.field, "field", "", "Print one credential field: url or password")
			cmd.Flags().StringVar(&o.format, "format", "", "Credential format: json or env (default json)")
		}
	}
	for _, name := range []string{"doctor", "sync"} {
		short := "Detect drift between saved state and PostgreSQL"
		if name == "sync" {
			short = "Repair managed objects; recreated databases are empty"
		}
		add(root, name+" [APP]", short, cobra.MaximumNArgs(1), func(ctx context.Context, r *runner, a []string) error { return r.health(ctx, name, a) })
	}
	showHelp := func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	for _, kind := range []string{"app", "db", "user"} {
		group := &cobra.Command{Use: kind, Short: "Manage " + kind + " objects", Args: cobra.NoArgs, RunE: showHelp}
		root.AddCommand(group)
		if kind == "app" {
			add(group, "create APP", "Create an empty logical app", cobra.ExactArgs(1), func(ctx context.Context, r *runner, a []string) error { return r.createApp(ctx, a[0], false) })
			add(group, "list", "List apps without passwords", cobra.NoArgs, list)
		}
		actions := []string{"add", "list", "delete"}
		if kind == "app" {
			actions = []string{"delete"}
		}
		if kind == "user" {
			actions = append(actions, "reset-password")
		}
		for _, action := range actions {
			target := "APP/DATABASE"
			if kind == "user" {
				target = "APP/USER"
			}
			if kind == "app" || action == "list" {
				target = "APP"
			}
			run := func(ctx context.Context, r *runner, a []string) error { return r.manage(ctx, kind, action, a[0]) }
			cmd := add(group, action+" "+target, action+" "+kind+" objects", cobra.ExactArgs(1), run)
			if action == "delete" {
				cmd.Flags().BoolVar(&o.force, "force", false, "Skip deletion confirmation; active connections still prevent deletion")
				if kind == "app" {
					alias := add(root, "delete APP", "Delete an app and all its databases and users", cobra.ExactArgs(1), run)
					alias.Flags().BoolVar(&o.force, "force", false, "Skip deletion confirmation")
				}
			}
			if kind == "user" && action == "add" {
				cmd.Flags().StringVar(&o.db, "db", "", "Database aliases, separated by commas")
				cmd.Flags().StringVar(&o.access, "access", "owner", "Access preset (only owner is supported)")
			}
		}
	}
	instance := &cobra.Command{Use: "instance", Short: "Register servers and choose the default instance", Args: cobra.NoArgs, RunE: showHelp}
	root.AddCommand(instance)
	add(instance, "list", "List registered instances without credentials", cobra.NoArgs, func(_ context.Context, r *runner, _ []string) error {
		return r.output(map[string]any{"default": r.s.state.Default, "instances": r.s.state.Instances})
	})
	for _, action := range []string{"add", "default"} {
		short := "Register a named PostgreSQL instance"
		if action == "default" {
			short = "Set the default PostgreSQL instance"
		}
		cmd := add(instance, action+" NAME", short, cobra.ExactArgs(1), func(ctx context.Context, r *runner, a []string) error { return r.instance(ctx, action, a[0]) })
		if action == "add" {
			f := cmd.Flags()
			f.StringVar(&o.host, "host", "localhost", "PostgreSQL host")
			f.IntVar(&o.port, "port", 5432, "PostgreSQL port")
			f.StringVar(&o.admin, "admin", "postgres", "Administrative PostgreSQL login")
			f.StringVar(&o.sslmode, "sslmode", "prefer", "TLS mode: disable, allow, prefer, require, verify-ca, verify-full")
			f.StringVar(&o.passwordEnv, "password-env", "PGPASSWORD", "Environment variable containing the admin password")
		}
	}
	return root
}
