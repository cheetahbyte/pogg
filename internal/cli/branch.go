package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var branchSlug = regexp.MustCompile(`[^a-z0-9]+`)

// branchDatabase fits within PostgreSQL's 63-byte identifier limit; the hash keeps similar branch names distinct.
func branchDatabase(db, branch string) string {
	slug := strings.Trim(branchSlug.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if len(slug) > 12 {
		slug = strings.TrimRight(slug[:12], "-")
	}
	if slug == "" {
		slug = "branch"
	}
	sum := sha256.Sum256([]byte(branch))
	return db + "__" + slug + "-" + hex.EncodeToString(sum[:3])
}
func checkBranchName(s string) error {
	if s == "" || len(s) > 200 || strings.HasPrefix(s, "-") || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("invalid branch name %q", s)
	}
	return nil
}

type gitContext struct{ repo, branch string }

// currentGit identifies the repository by its common git directory, so all worktrees share branch copies.
func currentGit(ctx context.Context) (gitContext, error) {
	b, e := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if e != nil {
		return gitContext{}, errors.New("not in a git repository; pass --branch NAME")
	}
	g := gitContext{repo: strings.TrimSpace(string(b))}
	if b, e = exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "--short", "HEAD").Output(); e == nil {
		g.branch = strings.TrimSpace(string(b))
	}
	return g, nil
}
func branchExists(ctx context.Context, repo, branch string) (bool, error) {
	if _, e := os.Stat(repo); errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	e := exec.CommandContext(ctx, "git", "--git-dir", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run()
	var exit *exec.ExitError
	if errors.As(e, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if e != nil {
		return false, fmt.Errorf("cannot read branches of %s", repo)
	}
	return true, nil
}

// selectedBranch returns --branch or the current git branch, and the repository to record.
func (r *runner) selectedBranch(ctx context.Context) (string, string, error) {
	g, gitErr := currentGit(ctx)
	if r.o.branch != "" {
		return r.o.branch, g.repo, checkBranchName(r.o.branch)
	}
	if gitErr != nil {
		return "", "", gitErr
	}
	if g.branch == "" {
		return "", "", errors.New("HEAD is detached; pass --branch NAME")
	}
	return g.branch, g.repo, checkBranchName(g.branch)
}

// branchTargets returns the selected database aliases: one for APP/DB, all for APP.
func branchTargets(name, key string, a *App) ([]string, error) {
	if key == "" {
		if len(a.Databases) == 0 {
			return nil, fmt.Errorf("app %q has no databases", name)
		}
		return keys(a.Databases), nil
	}
	if _, ok := a.Databases[key]; !ok {
		return nil, fmt.Errorf("unknown database alias %q; available: %s", key, strings.Join(keys(a.Databases), ", "))
	}
	return []string{key}, nil
}

// branchDatabaseFor resolves the physical database for credentials: --no-branch, --branch, or the current git branch.
func (r *runner) branchDatabaseFor(ctx context.Context, d Database) (string, string, error) {
	if r.o.noBranch {
		return d.Name, "", nil
	}
	if r.o.branch != "" {
		br, ok := d.Branches[r.o.branch]
		if !ok {
			return "", "", fmt.Errorf("database %s has no copy for branch %q; create one with pogg branch create", d.Name, r.o.branch)
		}
		return br.Name, r.o.branch, nil
	}
	if len(d.Branches) == 0 {
		return d.Name, "", nil
	}
	g, e := currentGit(ctx)
	if e != nil {
		return d.Name, "", nil
	}
	if br, ok := d.Branches[g.branch]; ok && br.Repo == g.repo {
		return br.Name, g.branch, nil
	}
	return d.Name, "", nil
}

func (r *runner) branch(ctx context.Context, action string, args []string) error {
	if action == "list" || action == "prune" {
		return r.branches(ctx, action, args)
	}
	name, key, a, e := r.app(args[0])
	if e != nil {
		return e
	}
	if e = deletionPending(a); e != nil {
		return e
	}
	targets, e := branchTargets(name, key, a)
	if e != nil {
		return e
	}
	branch, repo, e := r.selectedBranch(ctx)
	if e != nil {
		return e
	}
	label := args[0] + "@" + branch
	if action == "delete" {
		found := false
		for _, k := range targets {
			_, ok := a.Databases[k].Branches[branch]
			found = found || ok
		}
		if !found {
			return fmt.Errorf("no branch copies of %s for %q", args[0], branch)
		}
		if e = r.confirm(label); e != nil {
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
	if action == "delete" {
		for _, k := range targets {
			if e = r.dropBranch(ctx, c, name, k, branch); e != nil {
				return e
			}
		}
		return r.done("Deleted branch copies " + label)
	}
	for _, k := range targets {
		d := a.Databases[k]
		if _, ok := d.Branches[branch]; ok {
			return fmt.Errorf("database %s/%s already has a copy for %q", name, k, branch)
		}
		if e = vacant(ctx, c, "DATABASE", branchDatabase(d.Name, branch)); e != nil {
			return e
		}
	}
	for _, k := range targets {
		d := a.Databases[k]
		if d.Branches == nil {
			d.Branches = map[string]Branch{}
		}
		d.Branches[branch] = Branch{Name: branchDatabase(d.Name, branch), Repo: repo}
		a.Databases[k] = d
	}
	// Persist intent first so an interruption is recoverable via sync; undo it when copying fails.
	if e = r.s.save(); e != nil {
		return e
	}
	if e = r.s.reconcile(ctx, name, c); e != nil {
		for _, k := range targets {
			e = errors.Join(e, r.dropBranch(ctx, c, name, k, branch))
		}
		return e
	}
	return r.done("Created branch copies " + label)
}
func (r *runner) dropBranch(ctx context.Context, c *pgx.Conn, app, key, branch string) error {
	d := r.s.state.Apps[app].Databases[key]
	br, ok := d.Branches[branch]
	if !ok {
		return nil
	}
	if e := dropManagedDatabase(ctx, c, app, br.Name); e != nil {
		return e
	}
	delete(d.Branches, branch)
	return r.s.save()
}

type branchRow struct {
	App      string `json:"app"`
	Database string `json:"database"`
	Branch   string `json:"branch"`
	Name     string `json:"name"`
	Repo     string `json:"repo,omitempty"`
}

func (r *runner) branches(ctx context.Context, action string, args []string) error {
	names := keys(r.s.state.Apps)
	if len(args) == 1 {
		name, key, _, e := r.app(args[0])
		if e != nil {
			return e
		}
		if key != "" {
			return errors.New("expected APP")
		}
		names = []string{name}
	}
	rows := []branchRow{}
	for _, n := range names {
		a := r.s.state.Apps[n]
		for _, k := range keys(a.Databases) {
			d := a.Databases[k]
			for _, b := range keys(d.Branches) {
				br := d.Branches[b]
				if action == "prune" {
					if a.Deleting || d.Deleting || br.Repo == "" {
						continue
					}
					exists, e := branchExists(ctx, br.Repo, b)
					if e != nil {
						return e
					}
					if exists {
						continue
					}
				}
				rows = append(rows, branchRow{App: n, Database: k, Branch: b, Name: br.Name, Repo: br.Repo})
			}
		}
	}
	if action == "list" {
		return r.output(rows)
	}
	if len(rows) == 0 {
		return r.done("No stale branch copies.")
	}
	for _, row := range rows {
		fmt.Fprintf(r.errOut, "stale: %s/%s@%s (%s)\n", row.App, row.Database, row.Branch, row.Name)
	}
	if e := r.confirm(fmt.Sprintf("%d branch copies", len(rows))); e != nil {
		return e
	}
	for _, row := range rows {
		a := r.s.state.Apps[row.App]
		instance, e := r.appInstance(ctx, a)
		if e != nil {
			return e
		}
		c, e := r.s.admin(ctx, instance)
		if e != nil {
			return e
		}
		e = r.dropBranch(ctx, c, row.App, row.Database, row.Branch)
		c.Close(ctx)
		if e != nil {
			return e
		}
	}
	return r.done(fmt.Sprintf("Pruned %d branch copies", len(rows)))
}
