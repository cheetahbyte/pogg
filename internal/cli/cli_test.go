package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalContract(t *testing.T) {
	t.Setenv("PGPLEASE_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"a_b", "a/b", "a'", "", "UPPER", strings.Repeat("a", 21)} {
		if checkName(name) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	dir := os.Getenv("PGPLEASE_CONFIG_DIR")
	s, release, e := openStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = openStore(dir); e == nil {
		t.Fatal("concurrent lock allowed")
	}
	secret := "p@ss:/?# ' $\n"
	if e = s.secrets.Set("shop/api", secret); e != nil {
		t.Fatal(e)
	}
	s.state.Instances["local"] = Instance{Host: "::1", Port: 5432, Admin: "postgres", SSLMode: "disable"}
	s.state.Default = "local"
	s.state.Apps["shop"] = &App{Instance: "local", Databases: map[string]Database{"main": makeDatabase("shop", "main")}, Users: map[string]User{"api": {Name: "shop_api", Databases: []string{"main"}, SecretRef: "shop/api"}}}
	if e = s.save(); e != nil {
		t.Fatal(e)
	}
	release()
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if e != nil || bytes.Contains(b, []byte(secret)) {
		t.Fatal("state contains password", e)
	}
	info, e := os.Stat(filepath.Join(dir, "secrets.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("secret permissions", e)
	}
	run := func(args ...string) (string, string, error) {
		var out, errOut bytes.Buffer
		e := Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
		return out.String(), errOut.String(), e
	}
	raw, stderr, e := run("url", "shop")
	if e != nil || stderr != "" {
		t.Fatal(raw, stderr, e)
	}
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil {
		t.Fatal(e)
	}
	pword, _ := u.User.Password()
	if pword != secret || u.Host != "[::1]:5432" || strings.Count(raw, "\n") != 1 {
		t.Fatal("URL encoding", raw)
	}
	flagged, _, e := run("--on", "local", "url", "--db", "main", "shop", "--quiet")
	if e != nil || flagged != raw {
		t.Fatal("flags changed URL output", flagged, e)
	}
	structured, _, e := run("--json", "url", "shop")
	if e != nil || !json.Valid([]byte(structured)) {
		t.Fatal("global JSON flag", structured, e)
	}
	raw, _, e = run("get", "shop/api", "--format", "json")
	if e != nil || !json.Valid([]byte(raw)) {
		t.Fatal(raw, e)
	}
	raw, _, e = run("get", "shop", "--format", "env")
	if e != nil {
		t.Fatal(e)
	}
	// Verify env output is safely sourceable, including shell metacharacters and newlines.
	command := exec.Command("sh", "-c", raw+"\nprintf '%s' \"$PGPASSWORD\"")
	b, e = command.Output()
	if e != nil || string(b) != secret {
		t.Fatal("shell quoting", string(b), e)
	}
	for _, args := range [][]string{{"list"}, {"info", "shop"}, {"user", "list", "shop"}, {"db", "list", "shop"}, {"instance", "list"}} {
		raw, _, e = run(append(args, "--json")...)
		if e != nil || !json.Valid([]byte(raw)) || strings.Contains(raw, "p@ss") {
			t.Fatal(args, raw, e)
		}
	}
	if _, _, e = run("delete", "shop", "--json"); e == nil {
		t.Fatal("noninteractive delete accepted")
	}
	if e = os.Chmod(filepath.Join(dir, "secrets.json"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, _, e = run("url", "shop"); e == nil {
		t.Fatal("accepted exposed secrets file")
	}
	if e = os.Chmod(filepath.Join(dir, "secrets.json"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":1,"instances":{},"apps":{"broken":null}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = run("list"); e == nil {
		t.Fatal("accepted corrupt state")
	}
}

// Explicitly opt in: creates and destroys only a fresh, uniquely named Docker container.
func TestPostgresLifecycle(t *testing.T) {
	if os.Getenv("PGPLEASE_INTEGRATION") != "1" {
		t.Skip("set PGPLEASE_INTEGRATION=1 to test with disposable Docker PostgreSQL")
	}
	ctx := context.Background()
	container := fmt.Sprintf("pgplease-test-%d", time.Now().UnixNano())
	adminPass := password()
	b, e := exec.Command("docker", "run", "--detach", "--name", container, "-e", "POSTGRES_PASSWORD="+adminPass, "-p", "127.0.0.1::5432", "postgres:17-alpine").CombinedOutput()
	if e != nil {
		t.Fatalf("docker run: %s: %v", b, e)
	}
	t.Cleanup(func() {
		if b, e := exec.Command("docker", "rm", "-f", container).CombinedOutput(); e != nil {
			t.Logf("cleanup: %s: %v", b, e)
		}
	})
	i, _, e := inspect(ctx, container)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(40 * time.Second)
	for {
		c, e := connect(ctx, i, "postgres", i.Admin, adminPass)
		if e == nil {
			c.Close(ctx)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Setenv("PGPLEASE_CONFIG_DIR", t.TempDir())
	t.Setenv("PGPLEASE_TEST_PASSWORD", adminPass)
	run := func(args ...string) (string, error) {
		t.Helper()
		var out, errOut bytes.Buffer
		e := Run(ctx, args, strings.NewReader(""), &out, &errOut)
		if e != nil {
			return out.String(), fmt.Errorf("%w; stderr=%s", e, errOut.String())
		}
		return out.String(), nil
	}
	must := func(args ...string) string {
		t.Helper()
		out, e := run(args...)
		if e != nil {
			t.Fatalf("%v: %v; stdout=%s", args, e, out)
		}
		return out
	}
	must("instance", "add", "local", "--host", i.Host, "--port", fmt.Sprint(i.Port), "--admin", i.Admin, "--sslmode", "disable", "--password-env", "PGPLEASE_TEST_PASSWORD", "--json")
	must("create", "shop", "--json")
	c, e := connect(ctx, i, "postgres", i.Admin, adminPass)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	sql := func(q string) {
		t.Helper()
		if _, e := c.Exec(ctx, q); e != nil {
			t.Fatal(q, e)
		}
	}
	credential := func(target string, extra ...string) map[string]any {
		t.Helper()
		args := append([]string{"get", target, "--format", "json"}, extra...)
		var fields map[string]any
		if e := json.Unmarshal([]byte(must(args...)), &fields); e != nil {
			t.Fatal(e)
		}
		return fields
	}
	first := credential("shop")
	if first["username"] != "shop_shop" {
		t.Fatal(first)
	}
	userConn, e := connect(ctx, i, "shop", first["username"].(string), first["password"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = userConn.Exec(ctx, "CREATE TABLE widgets (id int)"); e != nil {
		t.Fatal(e)
	}
	userConn.Close(ctx)
	must("user", "add", "shop/api")
	second := credential("shop/api")
	userConn, e = connect(ctx, i, "shop", second["username"].(string), second["password"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = userConn.Exec(ctx, "ALTER TABLE widgets ADD COLUMN name text; INSERT INTO widgets VALUES (1,'test')"); e != nil {
		t.Fatal("shared migration ownership", e)
	}
	userConn.Close(ctx)
	if _, e = run("url", "shop"); e == nil {
		t.Fatal("ambiguous user accepted")
	}
	must("db", "add", "shop/events")
	must("user", "add", "shop/worker", "--db", "main,events")
	if _, e = run("url", "shop/worker"); e == nil {
		t.Fatal("ambiguous database accepted")
	}
	must("url", "shop/worker", "--db", "events")
	must("doctor", "--json")
	// Detect and repair ownership, privilege, schema, membership, and login drift.
	sql("ALTER ROLE shop_api CREATEDB; REVOKE pgplease_owner_shop_main FROM shop_api; GRANT CONNECT ON DATABASE shop TO PUBLIC")
	if _, e = run("doctor", "shop", "--json"); e == nil {
		t.Fatal("permission drift not detected")
	}
	must("sync", "shop", "--json")
	eventsAdmin, e := connect(ctx, i, "shop_events", i.Admin, adminPass)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = eventsAdmin.Exec(ctx, "DROP SCHEMA public"); e != nil {
		t.Fatal(e)
	}
	eventsAdmin.Close(ctx)
	if _, e = run("doctor", "shop", "--json"); e == nil {
		t.Fatal("missing schema not detected")
	}
	must("sync", "shop", "--json")
	must("user", "reset-password", "shop/api")
	reset := credential("shop/api")
	if reset["password"] == second["password"] {
		t.Fatal("password unchanged")
	}
	if old, e := connect(ctx, i, "shop", "shop_api", second["password"].(string)); e == nil {
		old.Close(ctx)
		t.Fatal("old password still works")
	}
	sql("CREATE ROLE stranger LOGIN")
	sql("CREATE DATABASE outside")
	output := must("doctor", "--json")
	if !strings.Contains(output, "unknown role stranger") || !strings.Contains(output, "unknown database outside") {
		t.Fatal(output)
	}
	sql("DROP DATABASE shop")
	sql("DROP ROLE shop_api")
	if _, e = run("doctor", "shop", "--json"); e == nil {
		t.Fatal("missing drift")
	}
	must("sync", "shop", "--json")
	after := credential("shop/api")
	if after["password"] != reset["password"] {
		t.Fatal("sync changed password")
	}
	must("doctor", "shop", "--json")
	userConn, e = connect(ctx, i, "shop", "shop_api", reset["password"].(string))
	if e != nil {
		t.Fatal(e)
	}
	var missing bool
	if e = userConn.QueryRow(ctx, "SELECT to_regclass('widgets') IS NULL").Scan(&missing); e != nil || !missing {
		t.Fatal("recreated db should be empty", e)
	}
	userConn.Close(ctx)
	// Refuse unknown object adoption, including an expected role replaced by an unrelated role.
	sql("DROP ROLE shop_api")
	sql("CREATE ROLE shop_api LOGIN")
	if _, e = run("sync", "shop"); e == nil {
		t.Fatal("adopted unmarked role")
	}
	sql("DROP ROLE shop_api")
	must("sync", "shop")
	if _, e = run("db", "delete", "shop/events", "--force"); e == nil {
		t.Fatal("deleted database still referenced by user")
	}
	must("user", "delete", "shop/worker", "--force")
	must("db", "delete", "shop/events", "--force")
	must("user", "delete", "shop/api", "--force")
	must("delete", "shop", "--force", "--json")
	var exists bool
	if e = c.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='stranger') AND EXISTS(SELECT 1 FROM pg_database WHERE datname='outside')").Scan(&exists); e != nil || !exists {
		t.Fatal("unknown objects changed", e)
	}
	// The advanced workflow and all list commands also emit parseable JSON.
	must("app", "create", "advanced")
	must("db", "add", "advanced/main")
	must("user", "add", "advanced/api", "--access", "owner")
	for _, args := range [][]string{{"list"}, {"app", "list"}, {"db", "list", "advanced"}, {"user", "list", "advanced"}, {"info", "advanced"}, {"get", "advanced/api"}, {"url", "advanced/api"}, {"instance", "list"}, {"doctor"}, {"sync"}} {
		out := must(append(args, "--json")...)
		if !json.Valid([]byte(out)) {
			t.Fatal(args, out)
		}
	}
	// A failed deletion is a tombstone, not an instruction for sync to recreate data.
	advanced := credential("advanced/api")
	busy, e := connect(ctx, i, "advanced", advanced["username"].(string), advanced["password"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = run("delete", "advanced", "--force"); e == nil {
		t.Fatal("deleted active database")
	}
	if _, e = run("sync", "advanced"); e == nil {
		t.Fatal("sync ignored pending deletion")
	}
	busy.Close(ctx)
	must("delete", "advanced", "--force")

	// Docker registration, endpoint refresh, and recovery after compose down -v.
	must("create", "dock", "--container", container)
	dockCred := credential("dock")
	if b, e = exec.Command("docker", "rm", "-f", container).CombinedOutput(); e != nil {
		t.Fatal(string(b), e)
	}
	newPassword := password()
	if b, e = exec.Command("docker", "run", "--detach", "--name", container, "-e", "POSTGRES_PASSWORD="+newPassword, "-p", "127.0.0.1::5432", "postgres:17-alpine").CombinedOutput(); e != nil {
		t.Fatal(string(b), e)
	}
	fresh, _, e := inspect(ctx, container)
	if e != nil {
		t.Fatal(e)
	}
	deadline = time.Now().Add(40 * time.Second)
	for {
		probe, err := connect(ctx, fresh, "postgres", fresh.Admin, newPassword)
		if err == nil {
			probe.Close(ctx)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	urlText := must("url", "dock")
	parsed, e := url.Parse(strings.TrimSpace(urlText))
	if e != nil || parsed.Port() != fmt.Sprint(fresh.Port) {
		t.Fatal("stale Docker endpoint", urlText, e)
	}
	if _, e = run("doctor", "dock", "--json"); e == nil {
		t.Fatal("recreated container drift not found")
	}
	must("sync", "dock", "--json")
	recovered := credential("dock")
	if recovered["password"] != dockCred["password"] {
		t.Fatal("lost stored password")
	}
	must("delete", "dock", "--force")
}
