package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func ident(s string) string    { return pgx.Identifier{s}.Sanitize() }
func literal(s string) string  { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func marker(app string) string { return "pgplease:" + app }
func object(ctx context.Context, c *pgx.Conn, kind, name string) (bool, string, error) {
	q := "SELECT COALESCE(shobj_description(oid, 'pg_authid'),'') FROM pg_roles WHERE rolname=$1"
	if kind == "DATABASE" {
		q = "SELECT COALESCE(shobj_description(oid, 'pg_database'),'') FROM pg_database WHERE datname=$1"
	}
	var tag string
	e := c.QueryRow(ctx, q, name).Scan(&tag)
	if e == pgx.ErrNoRows {
		return false, "", nil
	}
	return e == nil, tag, e
}
func execSQL(ctx context.Context, c *pgx.Conn, sql string) error {
	_, e := c.Exec(ctx, sql)
	if e != nil {
		if pe, ok := e.(interface{ SQLState() string }); ok {
			return fmt.Errorf("PostgreSQL operation failed (SQLSTATE %s)", pe.SQLState())
		}
		return fmt.Errorf("PostgreSQL operation failed")
	}
	return nil
}
func vacant(ctx context.Context, c *pgx.Conn, kind, name string) error {
	exists, _, e := object(ctx, c, kind, name)
	if e != nil {
		return e
	}
	if exists {
		return fmt.Errorf("%s %q already exists on server; refusing to adopt it", strings.ToLower(kind), name)
	}
	return nil
}
func managed(ctx context.Context, c *pgx.Conn, kind, name, app string) (bool, error) {
	exists, tag, e := object(ctx, c, kind, name)
	if e != nil {
		return false, e
	}
	if exists && tag != marker(app) {
		return true, fmt.Errorf("refusing to modify unmanaged %s %q", strings.ToLower(kind), name)
	}
	return exists, nil
}
func ensureRole(ctx context.Context, c *pgx.Conn, name, app, pass string, login bool) error {
	exists, e := managed(ctx, c, "ROLE", name, app)
	if e != nil {
		return e
	}
	if !exists {
		// Role creation and its provenance marker must commit together.
		tx, e := c.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, "CREATE ROLE "+ident(name)); e != nil {
			return fmt.Errorf("cannot create role %q", name)
		}
		if _, e = tx.Exec(ctx, "COMMENT ON ROLE "+ident(name)+" IS "+literal(marker(app))); e != nil {
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	attrs := " NOLOGIN"
	if login {
		attrs = " LOGIN PASSWORD " + literal(pass)
	}
	return execSQL(ctx, c, "ALTER ROLE "+ident(name)+" NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT"+attrs)
}
func (s *store) dbConn(ctx context.Context, instance, db string) (*pgx.Conn, error) {
	i := s.state.Instances[instance]
	p, e := s.secrets.Get(adminKey(instance))
	if e != nil {
		return nil, e
	}
	return connect(ctx, i, db, i.Admin, p)
}
func deletionPending(a *App) error {
	if a.Deleting {
		return fmt.Errorf("app deletion is incomplete; retry app delete --force")
	}
	for k, d := range a.Databases {
		if d.Deleting {
			return fmt.Errorf("database %s deletion is incomplete; retry db delete --force", k)
		}
	}
	return nil
}
func (s *store) reconcile(ctx context.Context, appName string, c *pgx.Conn) error {
	a := s.state.Apps[appName]
	if err := deletionPending(a); err != nil {
		return err
	}
	for _, key := range keys(a.Databases) {
		d := a.Databases[key]
		if e := ensureRole(ctx, c, d.Owner, appName, "", false); e != nil {
			return e
		}
		exists, e := managed(ctx, c, "DATABASE", d.Name, appName)
		if e != nil {
			return e
		}
		if !exists {
			if e = execSQL(ctx, c, "CREATE DATABASE "+ident(d.Name)+" OWNER "+ident(d.Owner)); e != nil {
				return e
			}
			if e = execSQL(ctx, c, "COMMENT ON DATABASE "+ident(d.Name)+" IS "+literal(marker(appName))); e != nil {
				return fmt.Errorf("database created but marking failed; inspect manually: %w", e)
			}
		}
		if e = execSQL(ctx, c, "ALTER DATABASE "+ident(d.Name)+" OWNER TO "+ident(d.Owner)); e != nil {
			return e
		}
		if e = execSQL(ctx, c, "REVOKE ALL ON DATABASE "+ident(d.Name)+" FROM PUBLIC"); e != nil {
			return e
		}
		dc, e := s.dbConn(ctx, a.Instance, d.Name)
		if e != nil {
			return e
		}
		e = execSQL(ctx, dc, "CREATE SCHEMA IF NOT EXISTS public AUTHORIZATION "+ident(d.Owner))
		if e == nil {
			e = execSQL(ctx, dc, "ALTER SCHEMA public OWNER TO "+ident(d.Owner))
		}
		if e == nil {
			e = execSQL(ctx, dc, "REVOKE CREATE ON SCHEMA public FROM PUBLIC")
		}
		dc.Close(ctx)
		if e != nil {
			return e
		}
	}
	for _, key := range keys(a.Users) {
		u := a.Users[key]
		p, e := s.secrets.Get(u.SecretRef)
		if e != nil {
			return e
		}
		if e = ensureRole(ctx, c, u.Name, appName, p, true); e != nil {
			return e
		}
		for _, db := range u.Databases {
			d, ok := a.Databases[db]
			if !ok {
				return fmt.Errorf("user %s references missing database %s", key, db)
			}
			if e = execSQL(ctx, c, "GRANT "+ident(d.Owner)+" TO "+ident(u.Name)); e != nil {
				return e
			}
			// All migration-created objects share the database owner, even across logins.
			if e = execSQL(ctx, c, "ALTER ROLE "+ident(u.Name)+" IN DATABASE "+ident(d.Name)+" SET role TO "+literal(d.Owner)); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *store) drift(ctx context.Context, name string, c *pgx.Conn) ([]string, error) {
	a := s.state.Apps[name]
	if err := deletionPending(a); err != nil {
		return []string{err.Error()}, nil
	}
	issues := []string{}
	check := func(kind, n string) (bool, error) {
		exists, tag, e := object(ctx, c, kind, n)
		if e != nil {
			return false, e
		}
		if !exists {
			issues = append(issues, "missing "+strings.ToLower(kind)+" "+n)
		} else if tag != marker(name) {
			issues = append(issues, "unmanaged collision "+strings.ToLower(kind)+" "+n)
		}
		if exists && kind == "ROLE" {
			var elevated, login, inherit bool
			if e = c.QueryRow(ctx, "SELECT rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls, rolcanlogin, rolinherit FROM pg_roles WHERE rolname=$1", n).Scan(&elevated, &login, &inherit); e != nil {
				return false, e
			}
			wantLogin := false
			for _, u := range a.Users {
				if u.Name == n {
					wantLogin = true
				}
			}
			if elevated || login != wantLogin || !inherit {
				issues = append(issues, "wrong role attributes for "+n)
			}
		}
		return exists, nil
	}
	for _, k := range keys(a.Databases) {
		d := a.Databases[k]
		if _, e := check("ROLE", d.Owner); e != nil {
			return nil, e
		}
		exists, e := check("DATABASE", d.Name)
		if e != nil {
			return nil, e
		}
		if !exists {
			continue
		}
		var owner string
		if e = c.QueryRow(ctx, "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname=$1", d.Name).Scan(&owner); e != nil {
			return nil, e
		}
		if owner != d.Owner {
			issues = append(issues, "wrong owner for database "+d.Name)
		}
		var publicAccess bool
		if e = c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database, LATERAL aclexplode(COALESCE(datacl,acldefault('d',datdba))) acl WHERE datname=$1 AND acl.grantee=0)", d.Name).Scan(&publicAccess); e != nil {
			return nil, e
		}
		if publicAccess {
			issues = append(issues, "public database privileges on "+d.Name)
		}
		dc, e := s.dbConn(ctx, a.Instance, d.Name)
		if e != nil {
			issues = append(issues, e.Error())
			continue
		}
		e = dc.QueryRow(ctx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname='public'").Scan(&owner)
		var publicCreate bool
		aclErr := dc.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace, LATERAL aclexplode(COALESCE(nspacl,acldefault('n',nspowner))) acl WHERE nspname='public' AND acl.grantee=0 AND acl.privilege_type='CREATE')").Scan(&publicCreate)
		dc.Close(ctx)
		if aclErr != nil {
			return nil, aclErr
		}
		if publicCreate {
			issues = append(issues, "public schema CREATE privilege in "+d.Name)
		}
		if e != nil || owner != d.Owner {
			issues = append(issues, "wrong or missing public schema owner in "+d.Name)
		}
	}
	for _, k := range keys(a.Users) {
		u := a.Users[k]
		exists, e := check("ROLE", u.Name)
		if e != nil {
			return nil, e
		}
		if !exists {
			continue
		}
		p, e := s.secrets.Get(u.SecretRef)
		if e != nil {
			issues = append(issues, e.Error())
			continue
		}
		for _, db := range u.Databases {
			d := a.Databases[db]
			var member bool
			e = c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid JOIN pg_roles u ON u.oid=m.member WHERE r.rolname=$1 AND u.rolname=$2)", d.Owner, u.Name).Scan(&member)
			if e != nil {
				return nil, e
			}
			if !member {
				issues = append(issues, "missing owner membership for "+u.Name+" on "+d.Name)
			}
			uc, e := connect(ctx, s.state.Instances[a.Instance], d.Name, u.Name, p)
			if e != nil {
				issues = append(issues, "login failed for "+u.Name+" on "+d.Name)
				continue
			}
			var current string
			e = uc.QueryRow(ctx, "SELECT current_user").Scan(&current)
			uc.Close(ctx)
			if e != nil || current != d.Owner {
				issues = append(issues, "wrong default role for "+u.Name+" on "+d.Name)
			}
		}
	}
	return issues, nil
}
func (s *store) unknown(ctx context.Context, instance string, c *pgx.Conn) ([]string, error) {
	roles := map[string]bool{s.state.Instances[instance].Admin: true}
	dbs := map[string]bool{"postgres": true, "template0": true, "template1": true}
	for _, a := range s.state.Apps {
		if a.Instance != instance {
			continue
		}
		for _, d := range a.Databases {
			dbs[d.Name] = true
			roles[d.Owner] = true
		}
		for _, u := range a.Users {
			roles[u.Name] = true
		}
	}
	warnings := []string{}
	for _, kind := range []string{"role", "database"} {
		q := "SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname"
		known := roles
		if kind == "database" {
			q = "SELECT datname FROM pg_database ORDER BY datname"
			known = dbs
		}
		rows, e := c.Query(ctx, q)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				rows.Close()
				return nil, e
			}
			if !known[n] {
				warnings = append(warnings, "unknown "+kind+" "+n+" (left untouched)")
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	return warnings, nil
}
func (s *store) dropUser(ctx context.Context, c *pgx.Conn, appName, key string) error {
	a := s.state.Apps[appName]
	u := a.Users[key]
	exists, e := managed(ctx, c, "ROLE", u.Name, appName)
	if e != nil || !exists {
		return e
	}
	for _, d := range a.Databases {
		exists, e := managed(ctx, c, "DATABASE", d.Name, appName)
		if e != nil {
			return e
		}
		if !exists {
			continue
		}
		dc, e := s.dbConn(ctx, a.Instance, d.Name)
		if e != nil {
			return e
		}
		e = execSQL(ctx, dc, "REASSIGN OWNED BY "+ident(u.Name)+" TO "+ident(d.Owner))
		if e == nil {
			e = execSQL(ctx, dc, "DROP OWNED BY "+ident(u.Name))
		}
		dc.Close(ctx)
		if e != nil {
			return e
		}
	}
	return execSQL(ctx, c, "DROP ROLE "+ident(u.Name))
}
func dropDatabase(ctx context.Context, c *pgx.Conn, appName string, d Database) error {
	exists, e := managed(ctx, c, "DATABASE", d.Name, appName)
	if e != nil {
		return e
	}
	if exists {
		if e = execSQL(ctx, c, "DROP DATABASE "+ident(d.Name)); e != nil {
			return fmt.Errorf("cannot drop database (close active connections first): %w", e)
		}
	}
	exists, e = managed(ctx, c, "ROLE", d.Owner, appName)
	if e != nil {
		return e
	}
	if exists {
		return execSQL(ctx, c, "DROP ROLE "+ident(d.Owner))
	}
	return nil
}
