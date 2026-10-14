# pogg

Provision PostgreSQL databases and owner logins for local development. Get connection URLs ready for scripts.

## Install

Requires Go 1.25+, PostgreSQL 14+, and an admin login. Docker discovery also requires the Docker CLI and a published PostgreSQL port.

```sh
go install ./cmd/pogg
```

Add Go's binary directory to your `PATH`.

## Quick start

Use an existing Docker container:

```sh
pogg create shop --container postgres
DATABASE_URL="$(pogg url shop)" npm run migrate
```

Or register a server with its admin password in `PGPASSWORD`:

```sh
pogg instance add local --host localhost --port 5432 --admin postgres
pogg instance default local
pogg create shop
```

Use `--on NAME` to select an instance. For remote servers with trusted certificates, add `--sslmode verify-full` when registering. `pogg` never creates, starts, or stops containers.

To disable TLS for an existing local instance:

```sh
pogg instance update local --sslmode disable
```

`update` changes only supplied settings and verifies the connection before saving. All apps on that instance inherit its SSL mode in their connection URLs. Supported flags are `--host`, `--port`, `--admin`, `--sslmode`, and `--password-env`. The saved admin password is reused unless you explicitly supply `--password-env`. This flag reads an existing server password; it doesn't reset the PostgreSQL password. Docker-backed instances allow only SSL mode and password updates; container discovery manages their connection address and admin login. Changing `--host` or `--port` on an instance that apps use requires `--force`, because it doesn't move their databases or users; use it only when the new address reaches the same server.

## Manage databases and users

`create shop` creates the app, its `main` database, and a `shop` user. Add more:

```sh
pogg db add shop/events
pogg user add shop/api --db main
pogg user add shop/worker --db main,events
pogg url shop/worker --db events
```

Only owner access is supported. Users can administer their assigned databases; this is not a multi-tenant security boundary.

## Retrieve credentials

```sh
pogg url shop/api
pogg get shop/api --field password
pogg get shop/api --format json
pogg get shop/api --format env
```

`url` prints only the URL; diagnostics go to stderr. Omit the user only for single-user apps. Use `--db` for users with multiple databases. Treat credential output as secrets.

## Branch databases

Give a git branch its own copy of the app's databases, for example while reviewing a pull request with migrations:

```sh
git switch feature/login
pogg branch create shop        # copies every database; use shop/events for one
pogg url shop                  # uses the copy while feature/login is checked out
pogg url shop --no-branch      # uses the source database
pogg url shop --branch feature/login

pogg branch list
pogg branch delete shop        # deletes the copies for the current branch
pogg branch prune              # deletes copies whose local branch no longer exists
```

- `url` and `get` use a copy only when one exists for the current branch of the same repository; they report it on stderr, even with `--quiet`.
- Copies share the source database's owner role, so every user with access to the source can use them.
- Copying requires that no other sessions are connected to the source database. Stop dev servers or other clients first.
- `sync` copies a missing branch database again from the source's current contents.
- Deleting a database or app also deletes its branch copies.

## Inspect, repair, and delete

```sh
pogg list
pogg info shop
pogg doctor
pogg sync shop
pogg user reset-password shop/api

pogg user delete shop/worker
pogg db delete shop/events
pogg delete shop
```

- `sync` repairs missing objects and access, not lost data. Recreated databases are empty.
- Deletion requires confirmation; `--force` skips it, but doesn't terminate active connections.
- Delete referencing users before deleting an individual database. App deletion removes all its databases and users.

## Configuration

Stored in `~/.config/pgplease` (or `$XDG_CONFIG_HOME/pgplease`). Override with `PGPLEASE_CONFIG_DIR`.

- `state.json`: managed objects and secret references.
- `secrets.json`: plaintext passwords, protected by mode `0600`.

Keep both files together. The old `pgplease` configuration, environment variable, and internal naming remain compatible; no migration is needed.

## Help

For coding agents, use the [using-pogg skill](skills/using-pogg/SKILL.md) for provisioning, credential handling, and repair approval rules.

```sh
pogg --help
pogg user add --help
pogg completion --help
```

## Test

```sh
go test ./...
go vet ./...
go build ./cmd/pogg

# Integration test: creates and removes a temporary PostgreSQL container.
PGPLEASE_INTEGRATION=1 go test ./internal/cli -run TestPostgresLifecycle -v
```
