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
