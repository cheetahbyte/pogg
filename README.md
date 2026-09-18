# pogg

The `pogg` CLI provisions PostgreSQL databases and owner logins for local development. Retrieve connection strings without parsing status messages. Previously named `pgplease`, the project now uses `pogg` for the CLI and Go module (`github.com/cheetahbyte/pogg`). Existing configuration paths, environment variable names, PostgreSQL owner roles, and managed-object markers retain their `pgplease` names for compatibility. No migration is required.

```sh
pogg create shop --container postgres
DATABASE_URL="$(pogg url shop)" npm run migrate
```

## Install

Requires Go 1.25 or later to build, PostgreSQL 14 or later, and an administrative PostgreSQL login. Docker is optional; when you use discovery, the Docker CLI must be available and the container's PostgreSQL port must be published to your machine.

From this repository, install the CLI:

```sh
go install ./cmd/pogg
```

Add Go's binary directory to your `PATH`. A Homebrew formula and published releases are not available yet.

## Connect to a server

For an existing local Docker container named `postgres`, create an app:

```sh
pogg create shop --container postgres
pogg url shop
```

With no saved default and exactly one running container publishing `5432/tcp`, you can omit `--container`. If several containers match, the CLI lists them and requires an explicit selection; it doesn't prompt in automation. Discovery reads `POSTGRES_USER` and `POSTGRES_PASSWORD`. Containers using `POSTGRES_PASSWORD_FILE`, unpublished ports, or a remote Docker daemon require explicit host registration instead.

For Homebrew PostgreSQL, Postgres.app, or a remote development server, register a named instance. Supply the admin password through `PGPASSWORD` in your environment, not a command-line argument:

```sh
pogg instance add local --host localhost --port 5432 --admin postgres
pogg instance default local
pogg create shop --on local
```

Use `--password-env NAME` for a different environment variable. An explicitly empty variable enables passwordless authentication. Direct instances default to `--sslmode prefer`; use `--sslmode verify-full` for a remote server with a trusted certificate. Docker discovery defaults to `disable` for local connections.

Instance selection uses `--instance` (alias `--on`) or `--container`, then the app's saved instance, then the saved default. Changing the default never moves existing apps. An explicit override for an existing app must match its instance.

Docker-backed connection strings refresh the published port using the stored container name. Administrative connections also try newly detected bootstrap credentials if the stored credentials stop working after container recreation. `doctor` rediscovers a default when its container is gone, but never silently rebinds existing apps to a different server. Restore a missing app container under its original name to use `sync` on that instance.

The CLI never starts, stops, or creates containers.

## Create apps, databases, and users

The shortcut creates an app named `shop`, a database alias `main` pointing to PostgreSQL database `shop`, and a user alias `shop` whose PostgreSQL role is `shop_shop`:

```sh
pogg create shop
pogg user add shop/api
pogg url shop/api
```

Create the full model explicitly:

```sh
pogg app create shop
pogg db add shop/main
pogg db add shop/events
pogg user add shop/api --db main --access owner
pogg user add shop/worker --db main,events
pogg url shop/worker --db events
```

These are alternative setup examples: don't run both against the same app name.

App, database alias, and user alias names use 1–20 lowercase letters, digits, or hyphens, starting with a letter. PostgreSQL names follow these rules:

| Object | PostgreSQL name |
|---|---|
| Main database | `APP` |
| Additional database | `APP_DATABASE` |
| Login role | `APP_USER` |
| Shared database owner | `pgplease_owner_APP_DATABASE` |

Only `owner` access is supported. Every user can administer its assigned databases. A shared, non-login owner role becomes the user's current role on connection, so migrations from different users own and can alter the same objects. New managed databases revoke public database privileges and public schema creation privileges. This is a development convenience, not a multi-tenant security boundary.

`ro`, `rw`, fine-grained grants, migrations, backups, container lifecycle management, and OS credential-store integration are outside this MVP.

## Retrieve credentials

`url` writes exactly one connection URL and a newline to stdout. Diagnostics go to stderr. You can omit the user only when the app has exactly one user. A user with several databases requires `--db`.

```sh
psql "$(pogg url shop/api)"
pogg get shop/api --field url
pogg get shop/api --field password
pogg get shop/api --format json
pogg get shop/api --format env
```

JSON includes the actual PostgreSQL username, database, host, port, password, URL, instance, and access preset. Environment output contains shell-quoted `DATABASE_URL`, `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, and `PGSSLMODE` assignments. Export variables yourself when sourcing them for child processes. Treat these outputs as secrets; don't put them in logs or version control.

Every application command accepts `--json`. For `url`, this explicitly switches from plain text to a JSON object. Lists and `info` use readable JSON even without the flag. Structured errors go to stderr; failed health checks emit their JSON report on stdout and exit nonzero. `--quiet` suppresses successful mutation status messages, not requested credentials or lists.

## Get help and shell completion

The CLI uses Cobra for command routing, argument validation, help, and shell completion. Global flags such as `--json` and `--instance` work before or after a subcommand. Flags such as `--format`, `--access`, and `--force` belong only to the commands that use them; unrelated flags are rejected.

Get help for a specific command:

```sh
pogg user add --help
pogg help instance add
```

Load completion for your current shell:

```sh
# Bash
source <(pogg completion bash)

# Zsh
source <(pogg completion zsh)
```

Fish and PowerShell completion are also available. Run `pogg completion --help` for instructions. Completion covers command and flag names, not saved app names or credentials. Help and completion don't open configuration files or connect to PostgreSQL; their output remains text even with `--json`.

## Inspect and delete objects

```sh
pogg list
pogg info shop
pogg db list shop
pogg user list shop
pogg instance list
```

List and info commands don't include passwords. Destructive commands require you to type the target name; automation must pass `--force`:

```sh
pogg user delete shop/worker --force
pogg db delete shop/events --force
pogg delete shop --force
```

Delete users referencing a database before deleting that database individually. App deletion removes all of its databases and users. Active database connections prevent deletion; `--force` bypasses confirmation, not PostgreSQL's active-connection protection. User deletion reassigns objects to the shared database owner before dropping the login.

Interrupted database or app deletions remain marked in state. Retry the delete command after resolving the failure. `sync` refuses to recreate an object pending deletion.

## Detect and repair drift

```sh
pogg doctor
pogg doctor shop --json
pogg sync shop --json
pogg user reset-password shop/api
```

`doctor` checks connectivity, managed-object markers, roles, database and public schema ownership, public privileges, memberships, and user logins. `sync` recreates missing roles, databases, and public schemas, restores owner access, and reapplies stored passwords. It doesn't recover deleted data. A recreated database is empty.

Objects found on the server but absent from state produce warnings. Neither command imports or deletes them. Provisioning refuses existing unregistered names. Managed objects carry PostgreSQL comments identifying their app; a missing or conflicting marker causes a refusal rather than adoption. Don't replace these comments. If a crash occurs between database creation and its marker write, inspect the database manually before repairing its marker or removing it.

Desired provisioning state is written before SQL operations so `sync` can finish an interrupted creation or password reset. PostgreSQL database creation and the two local files aren't one transaction. Keep both state files together when moving or recovering a configuration.

## Credential storage

The default directory is `~/.config/pgplease`, or `$XDG_CONFIG_HOME/pgplease` when set. Override it with `PGPLEASE_CONFIG_DIR` for an isolated configuration.

- `state.json`: instances, apps, database aliases, roles, and secret references. No passwords.
- `secrets.json`: generated user passwords and admin credentials, stored in plaintext with mode `0600`, comparable to `~/.pgpass`.
- `lock`: an exclusive, fail-fast lock for concurrent CLI processes.

New configuration directories use mode `0700`. File replacements are atomic. The CLI refuses a secrets file readable by other users. File-mode protection targets Unix-like systems; Windows ACL hardening and an OS credential backend are not implemented.

If a process crashes and leaves `lock`, first verify no `pogg` or older `pgplease` process is running, then remove that file. Never delete either JSON file to clear a lock.

## Test

Run unit tests, static checks, and build the CLI:

```sh
go test ./...
go vet ./...
go build ./cmd/pogg
```

Run the opt-in integration test:

```sh
PGPLEASE_INTEGRATION=1 go test ./internal/cli -run TestPostgresLifecycle -v
```

The integration test creates a uniquely named `postgres:17-alpine` container, uses a temporary configuration directory, and removes only that container afterward. It covers provisioning, multi-user migrations, multiple databases, credential output, password reset, unknown-object protection, drift repair, interrupted deletion, and Docker container recreation with changed ports and credentials.
