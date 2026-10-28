# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this project is

A self-hosted app that runs on a Synology NAS (DSM 7.2 or later) and backs up
**remote** MariaDB databases by taking SQL dumps on a schedule. The NAS is the
backup target; the databases live elsewhere. The app has a web UI for managing
connections, schedules, retention and file naming.

## Non-negotiable rules

These apply to every change. Do not weaken them for convenience.

1. **Never write to a target database.** The app is strictly read-only against
   every MariaDB server it has credentials for. No `INSERT`, `UPDATE`, `DELETE`,
   `CREATE`, `DROP`, `ALTER`, `GRANT`, `FLUSH`, `SET GLOBAL`, or anything else
   that mutates server state. The only statements the app itself may run are
   read-only probes (`SELECT 1`, `SELECT VERSION()`, `SHOW DATABASES`,
   `SHOW TABLES`). Dumps are produced by `mariadb-dump`, never by hand-written
   SQL. Never execute SQL supplied by a user or read from a file.
   - Open every connection with `SET SESSION TRANSACTION READ ONLY` as the
     first statement, as a belt-and-braces guard.
   - Use `--single-transaction` so dumps do not take table locks on the remote
     server. Do not pass `--lock-tables` or `--lock-all-tables`.
   - The README must tell users to create a dedicated DB user with only
     `SELECT, SHOW VIEW, TRIGGER, EVENT, PROCESS, LOCK TABLES`. Never suggest
     `ALL PRIVILEGES`.
2. **Credentials are stored as securely as possible.**
   - Passwords are encrypted at rest with AES-256-GCM. The data-encryption key
     is generated on first run, stored in its own file with mode `0600` outside
     the database file, and never committed, logged, or returned by any API.
   - Passwords are write-only in the API. Responses never include a stored
     password, not even masked. The edit form shows an empty field; leaving it
     empty keeps the existing password.
   - Never put a password on a command line (it would show in `ps`). Pass it
     to `mariadb-dump` via a temporary `--defaults-extra-file` created with
     mode `0600` and deleted in a `finally` block, or via a pipe. Never via
     `-p<password>`.
   - Never log passwords, connection strings containing passwords, or the
     contents of defaults files. Redact before logging. Error messages from
     the MariaDB client must be scrubbed of credentials before display.
   - Backup files and the app's own data directory must not be world-readable.
3. **Backups stay on the NAS.** Dump files are written to a directory on the
   device the app is installed on. No cloud upload, no remote push.
4. **Default MariaDB port is 3306.** Pre-fill it in the UI and in the API
   schema default.
5. **Test Connection is available on both the add and edit forms.** It must
   work before saving (using the values currently in the form, including an
   unsaved password) and must not persist anything. It runs read-only probes
   only and reports server version and whether the named database is visible.

## Functional requirements

### Connections

Each connection stores: display name, host, port (default 3306), database
name, username, password (encrypted), optional TLS flag. A connection can be
enabled or disabled without deleting it.

### Schedule

Per connection, or inherited from a global default:
- Frequency: hourly, every N hours, daily at a time, weekly on days at a time,
  or a raw cron expression for advanced users.
- Timezone taken from the host, shown in the UI.
- "Run now" button for an on-demand backup.
- Only one backup per connection runs at a time. A scheduled run that overlaps
  a running one is skipped and logged, not queued up.

### Retention

Per connection, or inherited from a global default:
- Keep the last N versions (count-based). Pruning runs after every successful
  backup and never deletes the file just written.
- A failed dump never triggers pruning and never overwrites a previous good
  file. Write to a temp name and rename on success.

### Filename options

Per connection, or inherited from a global default. The name is built from
parts, in order:

```
[timestamp_][prefix][base][suffix][_timestamp].sql[.gz]
```

A timestamp part is joined to the rest with `_`. Prefix and suffix are
concatenated directly, so users include their own separators.

- `base`: the database name by default, or a custom string.
- `prefix` / `suffix`: free text, optional.
- `timestamp_prefix` / `timestamp_suffix`: either, both, or neither. Format is
  configurable; default `YYYY-MM-DD_HHmmss`. A timestamp is required somewhere
  in the name unless the user explicitly opts into overwrite mode, because
  otherwise retention cannot distinguish versions.
- Show a live preview of the resulting filename in the form.
- Sanitise every part: strip path separators, `..`, control characters and
  leading dots. The final path must resolve inside the backup root.
- Optional gzip compression.

### Storage layout

```
<backup root>/<connection slug>/<filename>
```

The backup root is a directory on the NAS chosen at install time (on Docker,
a mounted volume). The app must fail loudly at startup if the root is missing
or not writable.

### History and status

Keep a per-run log: connection, start and end time, status, file written,
size, and the scrubbed error if it failed. Show the last result and next
scheduled run on the connections list.

## Platform decisions (assumptions, change if wrong)

- **Packaging:** ship as a Docker image with a `docker-compose.yml` for
  installation through DSM Container Manager. This avoids the Synology SPK
  toolchain and the `sc-*` package-user restrictions in DSM 7. A native SPK
  can be added later if wanted.
- **Image base:** Alpine with the `mariadb-client` package, which provides
  `mariadb-dump`. Pin the image version.
- **Volumes:** `/data` for the app database and key file, `/backups` for dump
  output. Both are bind-mounted from the NAS (for example
  `/volume1/docker/mariadb-backup/data` and `/volume1/backups/mariadb`).
- **Web UI auth:** a single admin login set on first run. Users are expected
  to put the UI behind DSM's reverse proxy for HTTPS. Sessions use secure,
  HttpOnly cookies.
- **Stack (decided):** Go, single static binary, `CGO_ENABLED=0` so it
  cross-compiles for both x86_64 and ARM64 Synology models.
  - HTTP: standard library `net/http` with Go 1.22+ method routing. No web
    framework.
  - UI: server-rendered `html/template` pages embedded in the binary, plus a
    small amount of vanilla JS for Test Connection, the filename preview and
    form toggles. No frontend build step, no npm.
  - Store: SQLite via `modernc.org/sqlite` (pure Go, no cgo).
  - Scheduling: `github.com/robfig/cron/v3`.
  - MariaDB probes: `github.com/go-sql-driver/mysql` (test connection only).
  - Crypto: standard library `crypto/aes` + `crypto/cipher` (AES-256-GCM);
    `golang.org/x/crypto/bcrypt` for the admin password.
  - Dumps: shell out to `mariadb-dump` from the Alpine `mariadb-client`
    package. The app never implements dumping itself.
  - Those are the only third-party dependencies. Adding one needs a reason.
- **Layout:** `cmd/server` is the entrypoint; everything else lives under
  `internal/` (`config`, `vault`, `store`, `model`, `schedule`, `naming`,
  `dumper`, `retention`, `scheduler`, `redact`, `web`).
- **Local dev on Windows:** Go lives at
  `%LOCALAPPDATA%\go-toolchain\go\bin` (not on PATH). `mariadb-dump` is not
  installed locally, so dumper integration tests are skipped unless the
  `MARIADB_TEST_*` env vars are set; CI runs them against a service container.

## Engineering conventions

- Small, focused modules. Separate: config, crypto, store, scheduler, dumper,
  retention, naming, HTTP API, UI.
- Every security-relevant function (encryption, filename sanitisation, path
  containment, credential redaction, read-only enforcement) has unit tests.
- Dumper is tested against a throwaway MariaDB container in CI, with a user
  that has only the read-only grants listed above. A test that needs more
  privileges is a bug.
- No secrets in the repo, in test fixtures, or in example compose files. Use
  placeholders like `change-me`.
- Log at `info` for runs and pruning, `warn` for skipped overlaps, `error`
  for failures. Logs go to stdout for Container Manager to collect.
- Keep the README current: install steps, required DB grants, volume layout,
  and how to restore a dump manually.

## Things not to do

- Do not add a "restore" feature that writes to a remote database. Restores
  are a manual, out-of-band action and the README explains how.
- Do not add remote or cloud destinations.
- Do not store the master key in the same SQLite file as the encrypted data.
- Do not bump the dependency surface for convenience features.
