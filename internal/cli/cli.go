package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func keys[V any](m map[string]V) []string {
	v := make([]string, 0, len(m))
	for k := range m {
		v = append(v, k)
	}
	sort.Strings(v)
	return v
}
func splitTarget(t string) (string, string, error) {
	p := strings.Split(t, "/")
	if len(p) > 2 {
		return "", "", errors.New("expected APP[/NAME]")
	}
	for _, n := range p {
		if e := checkName(n); e != nil {
			return "", "", e
		}
	}
	if len(p) == 1 {
		return p[0], "", nil
	}
	return p[0], p[1], nil
}
func configDir() (string, error) {
	if d := os.Getenv("PGPLEASE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "pgplease"), nil
	}
	h, e := os.UserHomeDir()
	return filepath.Join(h, ".config", "pgplease"), e
}

type runner struct {
	s           *store
	o           options
	in          io.Reader
	out, errOut io.Writer
}

func (r *runner) output(v any) error {
	enc := json.NewEncoder(r.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func (r *runner) done(message string) error {
	if r.o.quiet {
		return nil
	}
	if r.o.json {
		return r.output(map[string]any{"ok": true, "message": message})
	}
	_, e := fmt.Fprintln(r.out, message)
	return e
}
func (r *runner) confirm(target string) error {
	if r.o.force {
		return nil
	}
	if r.o.json {
		return errors.New("destructive action requires --force with --json")
	}
	fmt.Fprintf(r.errOut, "Delete %s? This cannot be undone. Type %q: ", target, target)
	text, e := bufio.NewReader(r.in).ReadString('\n')
	if (e != nil && !errors.Is(e, io.EOF)) || strings.TrimSpace(text) != target {
		return errors.New("deletion cancelled; automation must pass --force")
	}
	return nil
}
func (r *runner) app(target string) (string, string, *App, error) {
	a, k, e := splitTarget(target)
	if e != nil {
		return "", "", nil, e
	}
	app, ok := r.s.state.Apps[a]
	if !ok || app == nil {
		return "", "", nil, fmt.Errorf("unknown app %q", a)
	}
	return a, k, app, nil
}
func (r *runner) appInstance(ctx context.Context, a *App) (string, error) {
	var n string
	var e error
	if r.o.container != "" {
		n, e = r.s.byContainer(ctx, r.o.container)
	} else {
		n, e = r.s.resolve(ctx, r.o.instance, "", a.Instance)
	}
	if e != nil {
		return "", e
	}
	if n != a.Instance {
		return "", errors.New("app belongs to another instance; instance overrides cannot move apps")
	}
	return n, nil
}
func (r *runner) manage(ctx context.Context, cmd, action, target string) error {
	name, key, a, e := r.app(target)
	if e != nil {
		return e
	}
	if action == "list" {
		if key != "" {
			return errors.New("list expects APP")
		}
		if cmd == "db" {
			return r.output(a.Databases)
		}
		if cmd == "user" {
			return r.output(a.Users)
		}
	}
	if cmd == "app" && key != "" {
		return errors.New("expected APP")
	}
	if cmd != "app" && key == "" {
		return errors.New("expected APP/NAME")
	}
	if a.Deleting && !(cmd == "app" && action == "delete") {
		return errors.New("app deletion is incomplete; retry app delete --force")
	}
	if action == "delete" {
		if e = r.confirm(target); e != nil {
			return e
		}
	}
	instance, e := r.appInstance(ctx, a)
	if e != nil {
		return e
	}
	c, e := r.s.admin(ctx, instance)
	if e != nil {
		return e
	}
	defer c.Close(ctx)
	switch cmd + " " + action {
	case "db add":
		if _, ok := a.Databases[key]; ok {
			return errors.New("database already registered")
		}
		d := makeDatabase(name, key)
		if e = vacant(ctx, c, "DATABASE", d.Name); e != nil {
			return e
		}
		if e = vacant(ctx, c, "ROLE", d.Owner); e != nil {
			return e
		}
		a.Databases[key] = d
	case "user add":
		if _, ok := a.Users[key]; ok {
			return errors.New("user already registered")
		}
		dbs, e := chooseDBs(name, a, r.o.db)
		if e != nil {
			return e
		}
		u := User{Name: name + "_" + key, Databases: dbs, SecretRef: name + "/" + key}
		if e = vacant(ctx, c, "ROLE", u.Name); e != nil {
			return e
		}
		if e = r.s.secrets.Set(u.SecretRef, password()); e != nil {
			return e
		}
		a.Users[key] = u
	case "user reset-password":
		u, ok := a.Users[key]
		if !ok {
			return errors.New("unknown user")
		}
		if _, e = managed(ctx, c, "ROLE", u.Name, name); e != nil {
			return e
		}
		if e = r.s.secrets.Set(u.SecretRef, password()); e != nil {
			return e
		}
	case "user delete":
		u, ok := a.Users[key]
		if !ok {
			return errors.New("unknown user")
		}
		if e = r.s.dropUser(ctx, c, name, key); e != nil {
			return e
		}
		delete(a.Users, key)
		if e = r.s.save(); e != nil {
			return e
		}
		if e = r.s.secrets.Delete(u.SecretRef); e != nil {
			return e
		}
		return r.done("Deleted user " + target)
	case "db delete":
		d, ok := a.Databases[key]
		if !ok {
			return errors.New("unknown database")
		}
		for _, u := range a.Users {
			if slices.Contains(u.Databases, key) {
				return errors.New("delete users referencing this database first")
			}
		}
		d.Deleting = true
		a.Databases[key] = d
		if e = r.s.save(); e != nil {
			return e
		}
		if e = dropDatabase(ctx, c, name, d); e != nil {
			return e
		}
		delete(a.Databases, key)
		if e = r.s.save(); e != nil {
			return e
		}
		return r.done("Deleted database " + target)
	case "app delete":
		a.Deleting = true
		if e = r.s.save(); e != nil {
			return e
		}
		// Drop databases first; PostgreSQL refuses active connections rather than killing them.
		for _, k := range keys(a.Databases) {
			if e = dropDatabase(ctx, c, name, a.Databases[k]); e != nil {
				return e
			}
		}
		for _, k := range keys(a.Users) {
			if e = r.s.dropUser(ctx, c, name, k); e != nil {
				return e
			}
		}
		delete(r.s.state.Apps, name)
		if e = r.s.save(); e != nil {
			return e
		}
		for _, u := range a.Users {
			if e = r.s.secrets.Delete(u.SecretRef); e != nil {
				return e
			}
		}
		return r.done("Deleted app " + name)
	}
	// Persist intended objects before SQL; an interrupted provisioning is recoverable via sync.
	if e = r.s.save(); e != nil {
		return e
	}
	if e = r.s.reconcile(ctx, name, c); e != nil {
		return fmt.Errorf("desired state saved; run sync after resolving: %w", e)
	}
	return r.done("Updated " + target)
}
func makeDatabase(app, key string) Database {
	name := app + "_" + key
	if key == "main" {
		name = app
	}
	return Database{Name: name, Owner: "pgplease_owner_" + app + "_" + key}
}
func chooseDBs(app string, a *App, value string) ([]string, error) {
	if len(a.Databases) == 0 {
		return nil, fmt.Errorf("app %q has no databases; run: pogg db add %s/main", app, app)
	}
	if value == "" {
		if len(a.Databases) != 1 {
			return nil, fmt.Errorf("app %q has multiple databases; select with --db ALIAS (available: %s)", app, strings.Join(keys(a.Databases), ", "))
		}
		return keys(a.Databases), nil
	}
	names := strings.Split(value, ",")
	sort.Strings(names)
	names = slices.Compact(names)
	for _, n := range names {
		if _, ok := a.Databases[n]; !ok {
			return nil, fmt.Errorf("unknown database alias %q; available: %s; to create it, run: pogg db add %s/ALIAS", n, strings.Join(keys(a.Databases), ", "), app)
		}
	}
	return names, nil
}
func (r *runner) createApp(ctx context.Context, name string, simple bool) error {
	if e := checkName(name); e != nil {
		return e
	}
	if _, ok := r.s.state.Apps[name]; ok {
		return errors.New("app already exists")
	}
	n, e := r.s.resolve(ctx, r.o.instance, r.o.container, "")
	if e != nil {
		return e
	}
	a := &App{Instance: n, Databases: map[string]Database{}, Users: map[string]User{}}
	c, e := r.s.admin(ctx, n)
	if e != nil {
		return e
	}
	defer c.Close(ctx)
	if simple {
		d := makeDatabase(name, "main")
		u := User{Name: name + "_" + name, Databases: []string{"main"}, SecretRef: name + "/" + name}
		for kind, names := range map[string][]string{"DATABASE": {d.Name}, "ROLE": {d.Owner, u.Name}} {
			for _, v := range names {
				if e = vacant(ctx, c, kind, v); e != nil {
					return e
				}
			}
		}
		if e = r.s.secrets.Set(u.SecretRef, password()); e != nil {
			return e
		}
		a.Databases["main"] = d
		a.Users[name] = u
	}
	r.s.state.Apps[name] = a
	if e = r.s.save(); e != nil {
		return e
	}
	if e = r.s.reconcile(ctx, name, c); e != nil {
		return fmt.Errorf("desired state saved; run sync after resolving: %w", e)
	}
	message := "Created app " + name
	if !simple && !r.o.json {
		message += " (empty; no databases or users).\n\nNext:\n" + appSetupCommands(name, a)
	}
	return r.done(message)
}

func appSetupCommands(name string, a *App) string {
	db := "main"
	commands := ""
	if len(a.Databases) == 0 {
		commands = fmt.Sprintf("  pogg db add %s/%s\n", name, db)
	} else {
		db = keys(a.Databases)[0]
	}
	return commands + fmt.Sprintf("  pogg user add %s/api --db %s\n  pogg url %s/api", name, db, name)
}
func (r *runner) instance(ctx context.Context, action, name string) error {
	if e := checkInstanceName(name); e != nil {
		return e
	}
	if action == "default" {
		if _, ok := r.s.state.Instances[name]; !ok {
			return errors.New("unknown instance")
		}
		r.s.state.Default = name
		if e := r.s.save(); e != nil {
			return e
		}
		return r.done("Default instance: " + name)
	}
	if _, ok := r.s.state.Instances[name]; ok {
		return errors.New("instance already exists")
	}
	i := Instance{Host: r.o.host, Port: r.o.port, Admin: r.o.admin, SSLMode: r.o.sslmode}
	pass, hasPass := os.LookupEnv(r.o.passwordEnv)
	if r.o.container != "" {
		if r.o.explicitConn {
			return errors.New("--container discovers host, port, admin, and sslmode; do not combine with those flags")
		}
		var e error
		i, pass, e = inspect(ctx, r.o.container)
		if e != nil {
			return e
		}
		for n, old := range r.s.state.Instances {
			if old.Container == i.Container {
				return fmt.Errorf("container already registered as instance %q", n)
			}
		}
	} else if !hasPass {
		return fmt.Errorf("set %s to the admin password (an empty value explicitly enables passwordless auth)", r.o.passwordEnv)
	}
	if i.Host == "" || i.Admin == "" {
		return errors.New("host and admin must not be empty")
	}
	c, e := connect(ctx, i, "postgres", i.Admin, pass)
	if e != nil {
		return e
	}
	c.Close(ctx)
	if e = r.s.secrets.Set(adminKey(name), pass); e != nil {
		return e
	}
	r.s.state.Instances[name] = i
	if r.s.state.Default == "" {
		r.s.state.Default = name
	}
	if e = r.s.save(); e != nil {
		return e
	}
	return r.done("Registered instance " + name)
}
func (r *runner) credentials(ctx context.Context, cmd, target string) error {
	name, key, a, e := r.app(target)
	if e != nil {
		return e
	}
	if len(a.Users) == 0 {
		return fmt.Errorf("app %q has no users; create a login first:\n%s", name, appSetupCommands(name, a))
	}
	if key == "" {
		if len(a.Users) != 1 {
			return fmt.Errorf("app %q has multiple users; run: pogg %s %s/USER (available: %s)", name, cmd, name, strings.Join(keys(a.Users), ", "))
		}
		key = keys(a.Users)[0]
	}
	u, ok := a.Users[key]
	if !ok {
		return errors.New("unknown user")
	}
	if _, e = r.appInstance(ctx, a); e != nil {
		return e
	}
	db := r.o.db
	if db == "" {
		if len(u.Databases) != 1 {
			return errors.New("select a database with --db")
		}
		db = u.Databases[0]
	}
	if !slices.Contains(u.Databases, db) {
		return errors.New("user has no access to that database")
	}
	d, ok := a.Databases[db]
	if !ok {
		return errors.New("database missing from state")
	}
	if a.Deleting || d.Deleting {
		return errors.New("deletion is incomplete; retry the delete command")
	}
	i := r.s.state.Instances[a.Instance]
	if i.Container != "" {
		fresh, _, err := inspect(ctx, i.Container)
		if err != nil {
			return err
		}
		if fresh.Host != i.Host || fresh.Port != i.Port {
			i.Host = fresh.Host
			i.Port = fresh.Port
			r.s.state.Instances[a.Instance] = i
			if err = r.s.save(); err != nil {
				return err
			}
		}
	}
	p, e := r.s.secrets.Get(u.SecretRef)
	if e != nil {
		return e
	}
	v := connectionURL(i, d.Name, u.Name, p)
	fields := map[string]any{"url": v, "password": p, "host": i.Host, "port": i.Port, "database": d.Name, "username": u.Name, "instance": a.Instance, "access": "owner"}
	if r.o.json || r.o.format == "json" {
		if r.o.field != "" {
			return r.output(map[string]any{r.o.field: fields[r.o.field]})
		}
		if cmd == "url" {
			return r.output(map[string]string{"url": v})
		}
		return r.output(fields)
	}
	if r.o.field != "" {
		_, e = fmt.Fprintln(r.out, fields[r.o.field])
		return e
	}
	if r.o.format == "env" {
		for _, kv := range [][2]string{{"DATABASE_URL", v}, {"PGHOST", i.Host}, {"PGPORT", strconv.Itoa(i.Port)}, {"PGDATABASE", d.Name}, {"PGUSER", u.Name}, {"PGPASSWORD", p}, {"PGSSLMODE", i.SSLMode}} {
			if _, e = fmt.Fprintf(r.out, "%s=%s\n", kv[0], shellQuote(kv[1])); e != nil {
				return e
			}
		}
		return nil
	}
	if cmd == "get" {
		return r.output(fields)
	}
	_, e = fmt.Fprintln(r.out, v)
	return e
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func (r *runner) health(ctx context.Context, cmd string, p []string) error {
	if len(p) > 1 {
		return errors.New("expected doctor|sync [APP]")
	}
	names := keys(r.s.state.Apps)
	if len(p) == 1 {
		_, k, _, e := r.app(p[0])
		if e != nil {
			return e
		}
		if k != "" {
			return errors.New("expected APP")
		}
		names = p
	}
	rediscoveryWarning := ""
	if cmd == "doctor" && r.o.instance == "" && r.o.container == "" {
		var err error
		rediscoveryWarning, err = r.s.rediscoverDefault(ctx)
		if err != nil {
			rediscoveryWarning = err.Error()
		}
	}
	selected := ""
	var e error
	if r.o.instance != "" || r.o.container != "" {
		selected, e = r.s.resolve(ctx, r.o.instance, r.o.container, "")
		if e != nil {
			return e
		}
	}
	groups := map[string][]string{}
	for _, n := range names {
		a := r.s.state.Apps[n]
		if selected != "" && a.Instance != selected {
			if len(p) == 1 {
				return errors.New("app belongs to another instance")
			}
			continue
		}
		groups[a.Instance] = append(groups[a.Instance], n)
	}
	if len(p) == 0 {
		for n := range r.s.state.Instances {
			if selected == "" || selected == n {
				if _, ok := groups[n]; !ok {
					groups[n] = nil
				}
			}
		}
	}
	if len(groups) == 0 {
		n, e := r.s.resolve(ctx, r.o.instance, r.o.container, "")
		if e != nil {
			return e
		}
		groups[n] = nil
	}
	issues, warnings := []string{}, []string{}
	if rediscoveryWarning != "" {
		warnings = append(warnings, rediscoveryWarning)
	}
	for _, n := range keys(groups) {
		issues, warnings = r.checkInstance(ctx, cmd, n, groups[n], issues, warnings)
	}
	if r.o.json {
		if e = r.output(map[string]any{"ok": len(issues) == 0, "issues": issues, "warnings": warnings}); e != nil {
			return e
		}
	} else {
		for _, w := range warnings {
			fmt.Fprintln(r.errOut, "warning:", w)
		}
		for _, i := range issues {
			fmt.Fprintln(r.errOut, "drift:", i)
		}
		if len(issues) > 0 {
			fmt.Fprintln(r.errOut, "pogg:", ReportedError{}.Error())
		}
		if len(issues) == 0 && !r.o.quiet {
			fmt.Fprintln(r.out, "Managed state is healthy.")
		}
	}
	if len(issues) > 0 {
		return ReportedError{}
	}
	return nil
}

// One deadline per instance and per app: a whole doctor/sync run must not share a single 60s budget.
func (r *runner) checkInstance(parent context.Context, cmd, n string, apps, issues, warnings []string) ([]string, []string) {
	ctx, cancel := context.WithTimeout(parent, opTimeout)
	defer cancel()
	c, e := r.s.admin(ctx, n)
	if e != nil {
		return append(issues, n+": "+e.Error()), warnings
	}
	defer c.Close(ctx)
	for _, app := range apps {
		actx, acancel := context.WithTimeout(parent, opTimeout)
		if cmd == "sync" {
			if e = r.s.reconcile(actx, app, c); e != nil {
				issues = append(issues, app+": "+e.Error())
				acancel()
				continue
			}
		}
		found, e := r.s.drift(actx, app, c)
		acancel()
		if e != nil {
			issues = append(issues, app+": "+e.Error())
		} else {
			issues = append(issues, found...)
		}
	}
	found, e := r.s.unknown(ctx, n, c)
	if e != nil {
		return append(issues, n+": "+e.Error()), warnings
	}
	return issues, append(warnings, found...)
}

type ReportedError struct{}

func (ReportedError) Error() string {
	return "managed state has drift; run sync to repair missing objects (recreated databases are empty)"
}
