package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestOnboardingGuidance(t *testing.T) {
	var out bytes.Buffer
	root := newCommand(strings.NewReader(""), &out, &out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pogg create myapp", "pogg url myapp", "Use app create only for an empty app"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q", want)
		}
	}
	a := &App{Databases: map[string]Database{}, Users: map[string]User{}}
	r := &runner{s: &store{state: State{Apps: map[string]*App{"shop": a}}}}
	check := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	for _, cmd := range []string{"url", "get"} {
		check(r.credentials(context.Background(), cmd, "shop"), "pogg db add shop/main\n  pogg user add shop/api --db main\n  pogg url shop/api")
	}
	_, err := chooseDBs("shop", a, "")
	check(err, "has no databases; run: pogg db add shop/main")
	a.Databases["events"] = makeDatabase("shop", "events")
	if got := appSetupCommands("shop", a); got != "  pogg user add shop/api --db events\n  pogg url shop/api" {
		t.Fatal(got)
	}
	dbs, err := chooseDBs("shop", a, "")
	if err != nil || len(dbs) != 1 || dbs[0] != "events" {
		t.Fatal(dbs, err)
	}
	a.Databases["main"] = makeDatabase("shop", "main")
	_, err = chooseDBs("shop", a, "")
	check(err, "multiple databases; select with --db ALIAS (available: events, main)")
	_, err = chooseDBs("shop", a, "typo")
	check(err, "unknown database alias \"typo\"; available: events, main")
	a.Users["api"] = User{}
	a.Users["worker"] = User{}
	for _, cmd := range []string{"url", "get"} {
		check(r.credentials(context.Background(), cmd, "shop"), "pogg "+cmd+" shop/USER (available: api, worker)")
	}
}
