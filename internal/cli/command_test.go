package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandParsing(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "user", "add", "shop/api", "--db", "main,events", "--quiet"},
		{"user", "add", "--db=main,events", "--quiet", "shop/api", "--json"},
	} {
		var out bytes.Buffer
		root := newCommand(strings.NewReader(""), &out, &out)
		cmd, _, err := root.Find([]string{"user", "add"})
		if err != nil {
			t.Fatal(err)
		}
		called := false
		cmd.RunE = func(cmd *cobra.Command, pos []string) error {
			called = true
			json, _ := cmd.Flags().GetBool("json")
			quiet, _ := cmd.Flags().GetBool("quiet")
			db, _ := cmd.Flags().GetString("db")
			if !json || !quiet || db != "main,events" || !slices.Equal(pos, []string{"shop/api"}) {
				t.Fatalf("wrong parsing: json=%v quiet=%v db=%s args=%v", json, quiet, db, pos)
			}
			return nil
		}
		root.SetArgs(args)
		if err = root.Execute(); err != nil || !called {
			t.Fatal(args, err)
		}
	}
}

func TestHelpValidationAndCompletionDoNotOpenState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uncreated")
	t.Setenv("PGPLEASE_CONFIG_DIR", dir)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "Available Commands:"},
		{[]string{"user", "add", "--help"}, "--access"},
		{[]string{"help", "instance", "add"}, "--password-env"},
		{[]string{"user"}, "reset-password"},
		{[]string{"completion", "bash"}, "bash completion"},
		{[]string{"completion", "zsh"}, "#compdef pogg"},
		{[]string{"__complete", "user", ""}, "reset-password"},
	} {
		var out, stderr bytes.Buffer
		err := Run(context.Background(), tc.args, strings.NewReader(""), &out, &stderr)
		if err != nil || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v: %v; stdout=%s; stderr=%s", tc.args, err, &out, &stderr)
		}
	}
	for _, args := range [][]string{
		{"bogus"}, {"user", "bogus"}, {"create"}, {"list", "extra"}, {"url", "a", "b"},
		{"user", "add", "shop/api", "--access", "rw"},
		{"instance", "add", "local", "--port", "0"},
		{"instance", "add", "local", "--sslmode", "bogus"},
		{"get", "shop", "--format", "xml"}, {"get", "shop", "--field", "bogus"},
		{"get", "shop", "--field", "url", "--format", "json"},
		{"get", "shop", "--json", "--format", "env"},
		{"--unknown"}, {"--instance"}, {"list", "--json=garbage"},
		{"list", "--force"}, {"url", "shop", "--format", "env"},
		{"create", "shop", "--instance", "local", "--container", "pg"},
	} {
		var out, stderr bytes.Buffer
		err := Run(context.Background(), args, strings.NewReader(""), &out, &stderr)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
		if out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("Cobra printed usage/errors instead of returning to the caller: %v stdout=%s stderr=%s", args, &out, &stderr)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("help, completion, or invalid input accessed state: %v", err)
	}
}
