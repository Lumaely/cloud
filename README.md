# Lumaely Cloud

Go API that stores the end-to-end encrypted Lumaely diary: PostgreSQL for records, MinIO (S3) for files. The API contract is in [src/docs/API.md](src/docs/API.md), with an OpenAPI copy in [src/docs/openapi.yaml](src/docs/openapi.yaml) for generating clients.

```mermaid
flowchart LR
  App[Lumaely app] -->|HTTPS| API[cloud API :8080]
  API --> DB[(PostgreSQL)]
  API --> S3[(MinIO / S3)]
```

## Layout

```
.github/            CI workflow and issue templates
src/
  main.go           entry point: runs app.Run
  app/              configuration, routes, migrations, server commands
  handlers/  middleware/  models/  services/
  db/migrations/    goose SQL migrations, compiled into the binary
  scripts/          start_dev, build_prod, start_prod, apply_migration
  test/             integration and script tests (real Postgres and MinIO)
  docs/             API contract: API.md, openapi.yaml
Dockerfile          production image
Dockerfile.dev      dev image with live reload
docker-compose.yml  production stack
docker-compose-dev.yml
go.mod  go.sum  README.md  .env.sample
```

Unit tests sit next to the code they test, because Go only lets them reach a package's private functions from the same folder. Build the binary with `go build -o main ./src`.

## Setup

Requirements: Docker with Compose v2. Go 1.27 only for `--local` migrations and tests.

```bash
cp .env.sample .env
```

Fill `.env`, then use the scripts below. Compose reads it: wrap a value containing `$` in single quotes (`DB_PASSWORD='ab$cd'`).

Upgrading a checkout that still runs the `cloud` Compose project (containers and volumes named `cloud_*`):

1. Stop its containers: `docker compose -f docker-compose-dev.yml -p cloud down`.
2. To keep that data, copy the `cloud_postgres_data` and `cloud_minio_data` volumes into `lumaely_postgres_data` and `lumaely_minio_data` before the first `start_prod`.
3. Recreate a dev database whose schema came from `db/init/01_schema.sql`: `docker compose -f docker-compose-dev.yml down -v`. goose would otherwise mark it as migration 1 whatever its tables are.
4. Recreate the same way a dev database migrated before the current `00001_baseline.sql` (it lacks the `records` and `attachments` tables). goose sees migration 1 as applied and prints `no migration to apply`; sign-in, sync and file routes then return 500.

## Scripts

All scripts live in `src/scripts/`, run from any folder, and print their usage with `--help`. `start_dev`, `start_prod` and `apply_migration --dev`/`--prod` need `.env` at the repository root and stop with exit code 1 when it is missing; `build_prod` and `apply_migration --local` do not.

| Script | Does |
|---|---|
| `start_dev` | Starts the dev stack with live reload. |
| `build_prod` | Builds the production image. |
| `start_prod` | Starts the production stack from a built image and waits until it is healthy. |
| `apply_migration` | Applies or lists database migrations. |

Dev and prod are separate Compose projects (`lumaely-dev`, `lumaely`) with separate containers and volumes. Both publish the API on port 8080, so run one at a time.

### `start_dev [compose options]`

```bash
src/scripts/start_dev        # foreground, logs in the terminal
src/scripts/start_dev -d     # background
```

- Builds `Dockerfile.dev` and runs `docker compose -f docker-compose-dev.yml up --build` with your options.
- Starts `db` (Postgres, port 5432), `s3` (MinIO API port 9000, console port 9001) and `app`.
- `app` waits for healthy `db` and `s3`, applies pending migrations, then serves on http://localhost:8080.
- `app` rebuilds and restarts on every file change (air).
- Stop it with `docker compose -f docker-compose-dev.yml down`; add `-v` to erase the dev database and files.

### `build_prod [VERSION]`

```bash
src/scripts/build_prod           # version from git describe
src/scripts/build_prod 1.2.0
```

- Builds `Dockerfile` and tags `lumaely-cloud:VERSION` and `lumaely-cloud:latest`.
- `VERSION` defaults to `git describe --tags --always --dirty`, or `dev` outside a git checkout.
- The version is compiled in: `/health` and `docker run --rm lumaely-cloud:VERSION version` print it.

### `start_prod [VERSION]`

```bash
src/scripts/start_prod           # lumaely-cloud:latest
src/scripts/start_prod 1.2.0
```

- Stops with exit code 1, before starting anything, when:
  - `.env` is missing, or `curl` is not installed;
  - any of `DB_USER`, `DB_PASSWORD`, `DB_NAME`, `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD`, `SERVER_SECRET` is empty: Compose names the variable;
  - the image `lumaely-cloud:VERSION` does not exist (it is never pulled: run `build_prod` first).
- The image is always `VERSION` or `latest`; a `LUMAELY_VERSION` in `.env` is ignored.
- Runs `docker compose -f docker-compose.yml up -d` with that image. Only the API port 8080 is published.
- `app` applies pending migrations before serving.
- Polls http://localhost:8080/health every 2 s for up to 120 s:
  - healthy: prints the health answer, exit code 0;
  - healthy but reporting another version than an explicit `VERSION` (another server holds port 8080): exit code 1;
  - not healthy: prints the last 50 app log lines, exit code 1. The stack keeps running for inspection.
- Stop it with `docker compose -f docker-compose.yml down`. Never add `-v` in production: it erases the database and files.
- Every `docker compose -f docker-compose.yml` command, `down` and `logs` included, needs the filled `.env`: Compose checks the required variables first.

### `apply_migration [--dev | --prod | --local] [--status | TARGET]`

```bash
src/scripts/apply_migration                       # every pending migration, dev stack
src/scripts/apply_migration 00003_add_tags.sql    # up to and including 00003
src/scripts/apply_migration 3                     # same, by version
src/scripts/apply_migration --status              # list, change nothing
src/scripts/apply_migration --prod                # in the prod stack
```

| Argument | Effect |
|---|---|
| none | Applies every pending migration. Prints `no migration to apply` when there is none. |
| `TARGET` | Applies every pending migration up to and including `TARGET`: a file name, a path (only its file name is used) or a version number. |
| `--status` | Prints one line per migration, `Applied <date> <file>` or `Pending - <file>`. |
| `--dev` (default) | Runs `go run ./src migrate` in a one-off `app` container of the dev stack; starts `db` and `s3` if needed. |
| `--prod` | Runs `/main migrate` in a one-off container of the image `lumaely-cloud:$LUMAELY_VERSION` (default `latest`). Set `LUMAELY_VERSION` to the version you run, e.g. `LUMAELY_VERSION=1.2.0 src/scripts/apply_migration --prod --status`; `start_prod` already migrates, so this is mostly for `--status`. |
| `--local` | Runs `go run ./src migrate` on this machine. `DATABASE_URL`, or the `DB_*` values, select the database; `MIGRATIONS_DIR` can replace the built-in migrations. Does not need `.env`. |

Exit code 0 on success. Exit code 1 when:

- `TARGET` is not a known migration (`unknown migration "…"`): nothing is applied;
- `TARGET` is already applied (`migration … is already applied`): nothing is applied;
- a migration fails: its transaction is rolled back; migrations applied before it in the same run stay applied.

Exit code 2, before anything runs, for an unknown option, two targets, or `--status` with a target.

## Database migrations

Migrations are SQL files in `src/db/migrations/`, applied in version order by [goose](https://github.com/pressly/goose) and compiled into the binary.

```mermaid
flowchart LR
  S[server or apply_migration starts] --> L[Postgres advisory lock]
  L --> V[read goose_db_version]
  V --> P{pending files?}
  P -->|yes| T[run each in a transaction, record its version]
  P -->|no| U[unlock]
  T --> U
```

- The server applies every pending migration at startup and refuses to start if one fails.
- The lock lets only one process migrate at a time, so several servers can start together.
- `goose_db_version` records applied versions; never edit it by hand.

Rules for a new migration:

1. Create `src/db/migrations/NNNNN_short_name.sql` with the next 5-digit number.
2. Write `-- +goose Up` with the change, and `-- +goose Down` with its inverse.
3. Never edit or rename a migration once it is on `main`; fix it with a new one.
4. Test it with `src/scripts/apply_migration --local` against a test database, then `--dev`.

```sql
-- +goose Up
ALTER TABLE patterns ADD COLUMN archived_at TIMESTAMPTZ NULL;

-- +goose Down
ALTER TABLE patterns DROP COLUMN archived_at;
```

## Configuration

Set in `.env` (see `.env.sample`). Compose sets `DB_HOST`, `DB_PORT` and `MINIO_ENDPOINT` itself.

| Variable | Default | Use |
|---|---|---|
| `DATABASE_URL` | empty | Full Postgres URL; replaces the `DB_*` values. Host only: Compose blanks it in containers. |
| `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME` | `localhost`, `5432`, `Admin`, dev password, `Lumaely-db` | Postgres connection. |
| `MINIO_ENDPOINT`, `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD`, `MINIO_BUCKET`, `MINIO_REGION`, `MINIO_USE_SSL` | `localhost:9000`, `minioadmin`, `minioadmin`, `journal-attachments`, `us-east-1`, `false` | S3 storage. |
| `SERVER_MODE` | `cloud` | `cloud` or `self-hosted` (no email routes, no quota). |
| `SERVER_SECRET` | random per start | Keeps fake login salts stable; required in production. |
| `STORAGE_QUOTA_BYTES` | 5 GiB in cloud mode, 0 (unlimited) in self-hosted | Per-account file quota. Previews are not counted; each is at most 256 KiB and, when the quota is not 0, at most the size of its file. |
| `MAIL_DRIVER`, `MAIL_FROM` | `log`, `Lumaely <no-reply@lumaely.app>` | Email sending; `log` writes emails to the server log, `scaleway` sends through Scaleway Transactional Email. |
| `SCW_SECRET_KEY`, `SCW_PROJECT_ID`, `SCW_REGION` | empty, empty, `fr-par` | Scaleway credentials; required when `MAIL_DRIVER=scaleway`. |
| `TRUSTED_PROXIES` | none | Proxy IPs/CIDRs, so rate limits see client IPs. |
| `MIGRATIONS_DIR` | empty (built-in) | Folder of migrations to use instead of the built-in ones. Host only (`--local`, tests): Compose blanks it in containers. |

## Server commands

The binary takes one command: `serve` (default), `migrate [up | up-to TARGET | status]`, `version`. `pair` is reserved for self-hosted pairing and not available yet.

## Tests

```bash
go test ./...
```

Unit tests always run. Integration tests in `src/test/`, including the `apply_migration` script tests, need a disposable Postgres database and MinIO, and erase that database:

```bash
TEST_DATABASE_URL="postgres://USER:PASSWORD@localhost:5432/lumaely_test?sslmode=disable" \
MINIO_TEST_ENDPOINT=localhost:9000 MINIO_TEST_ACCESS_KEY=minioadmin MINIO_TEST_SECRET_KEY=minioadmin \
go test ./...
```

CI (`.github/workflows/tests.yaml`) runs shellcheck on the scripts, vet, and every test with Postgres and MinIO containers on each push. A new push to a branch cancels its running workflow.
