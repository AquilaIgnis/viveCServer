# viveCServer

The sync server for [viveNotes](https://github.com/AquilaIgnis/viveNotes) — Go and PostgreSQL,
self-hostable as a single binary.

It stores, orders and authorises. It never interprets a note: page documents are opaque payloads,
and everything that needs the document model runs on the client, which already has it. The design
and its reasoning are in `memory/syncPlan.md`.

**Status: S1.** Skeleton, configuration, migrations, health endpoints, accounts, device registration
and token authentication. The sync protocol itself starts at S2.

## Running it

```bash
docker compose -f deploy/docker-compose.yml up --build
curl localhost:8080/healthz
curl localhost:8080/readyz
```

## First account

Registration over HTTP is closed by default, so the first account is created with a subcommand of
the same binary:

```bash
printf 'your-password\n' | docker compose -f deploy/docker-compose.yml run --rm -T \
  server create-account -email you@example.com
```

The password comes from stdin, or from `VIVE_ACCOUNT_PASSWORD`. Prefer stdin: an environment
variable is visible in `docker inspect`, in a committed compose file, and in `/proc` on the host.

## API

| | |
| --- | --- |
| `POST /v1/accounts` | `{email, password}` → `{accountId}`. **403 unless `VIVE_SIGNUP_MODE=open`.** |
| `POST /v1/devices` | `{email, password, name, platform}` → `{deviceId, accountId, token}` |
| `GET /v1/devices` | the account's devices — requires a token |
| `DELETE /v1/devices/{id}` | revoke, including the calling device — requires a token |

Authenticate with `Authorization: Bearer vive_…`. **The token is returned exactly once**; only its
SHA-256 is stored, so a client that loses it registers again. Revocation takes effect on the next
request — there is no session cache to invalidate.

Errors are `{"error": "<code>", "message": "…"}` with a closed set of codes: `invalid_request`,
`invalid_credentials`, `signup_closed`, `email_taken`, `unauthenticated`, `not_found`,
`payload_too_large`, `internal`. Branch on the code, never the message.

Or against a PostgreSQL you already have:

```bash
export VIVE_DATABASE_URL='postgres://user:password@localhost:5432/vive'
go run ./cmd/vivecserver
```

## Configuration

Read from the environment, or from a `.env` file beside the binary. A real environment variable
always wins over the file. Everything is validated at startup, so a bad setting is one line of
output at boot rather than a surprise on the first request.

| Variable | Default | |
| --- | --- | --- |
| `VIVE_DATABASE_URL` | — | **Required.** PostgreSQL connection string. |
| `VIVE_LISTEN_ADDR` | `:8080` | |
| `VIVE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `VIVE_SIGNUP_MODE` | `closed` | `closed` or `open`. An unrecognised value is refused rather than defaulted — the difference between the two is the difference between a private server and a public one. |
| `VIVE_SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight requests. |

## Health

`/healthz` is liveness and checks nothing but the process. `/readyz` is readiness and pings the
database. They are separate because a liveness probe that touches the database turns a brief
database hiccup into a restart loop. Point container restarts at the first and load balancers at
the second.

## Migrations

SQL files in `migrations/`, embedded into the binary and applied by goose on startup under a
PostgreSQL advisory lock, so two instances starting together cannot both apply the same migration.

The goose CLI reads the same files for the things a CLI is better at:

```bash
export GOOSE_DRIVER=postgres
export GOOSE_DBSTRING="$VIVE_DATABASE_URL"
export GOOSE_MIGRATION_DIR=migrations

goose status
goose create add_something sql   # then rename to the next 5-digit sequential version
goose down
```

**A released migration is never edited.** goose records which versions ran, not what they contained,
so editing one leaves every database that ran the old text silently different from every database
that runs the new one. Add a migration instead.

## Licence

Source First 1.1, matching viveNotes.
