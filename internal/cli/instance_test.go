package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerResolution(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  inspect) [ "$4" = gone ] && exit 1; printf '%s' "$PGPLEASE_TEST_INSPECT" ;;
  ps) if [ "$2" = -a ]; then printf '%s' "$PGPLEASE_TEST_ALL"; else printf '%s' "$PGPLEASE_TEST_RUNNING"; fi ;;
  *) exit 1 ;;
esac
`
	if e := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PGPLEASE_TEST_INSPECT", `[{"Name":"/found","State":{"Running":true},"Config":{"Env":["POSTGRES_USER=admin","POSTGRES_PASSWORD=secret"]},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"54321"}]}}}]`)
	t.Setenv("PGPLEASE_TEST_RUNNING", "found")
	t.Setenv("PGPLEASE_TEST_ALL", "found")
	s, release, e := openStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	ctx := context.Background()
	n, e := s.resolve(ctx, "", "", "")
	if e != nil || n != "found" || s.state.Default != "found" {
		t.Fatal(n, e)
	}
	i := s.state.Instances[n]
	if i.Host != "127.0.0.1" || i.Port != 54321 || i.Admin != "admin" {
		t.Fatal(i)
	}
	p, e := s.secrets.Get(adminKey(n))
	if e != nil || p != "secret" {
		t.Fatal(p, e)
	}
	t.Setenv("PGPLEASE_TEST_RUNNING", "one\ntwo")
	if n, e = s.resolve(ctx, "", "", ""); e != nil || n != "found" {
		t.Fatal("saved default not honored", n, e)
	}
	s.state.Default = ""
	if _, e = s.resolve(ctx, "", "", ""); e == nil || !strings.Contains(e.Error(), "found 2") {
		t.Fatal("ambiguous discovery accepted", e)
	}
	s.state.Instances["old"] = Instance{Host: "localhost", Port: 5432, Admin: "postgres", SSLMode: "disable", Container: "gone"}
	s.state.Default = "old"
	s.state.Apps["shop"] = &App{Instance: "old", Databases: map[string]Database{}, Users: map[string]User{}}
	t.Setenv("PGPLEASE_TEST_RUNNING", "found")
	warning, e := s.rediscoverDefault(ctx)
	if e != nil || warning == "" || s.state.Default != "found" || s.state.Apps["shop"].Instance != "old" {
		t.Fatal("unsafe rediscovery", warning, e)
	}
}
