# Synology MariaDB Backup

A small self-hosted app for a Synology NAS (DSM 7.2+) that backs up **remote**
MariaDB databases on a schedule using `mariadb-dump`, and keeps the dump files
on the NAS.

- Web UI to add database connections, with **Test connection** on both the add
  and edit forms.
- Per-connection or global **schedule** (hourly, every N hours, daily, weekly,
  or cron), **retention** (keep the last N versions) and **filename** rules
  (database name or custom base, prefix, suffix, timestamp prefix/suffix with
  a configurable format, optional gzip).
- **Read-only** against your databases. The app only ever runs `SELECT`,
  `SHOW` and `mariadb-dump --single-transaction`. It has no restore feature by
  design.
- Passwords are encrypted at rest with AES-256-GCM using a key file that is
  created on first run and never leaves the data volume. Passwords are never
  shown again in the UI or API, never logged, and never put on a command line.

## Install on DSM (Container Manager)

1. Open **File Station** and create two shared-folder paths:
   - `/volume1/docker/mariadb-backup/data` – app database and encryption key
   - `/volume1/backups/mariadb` – where the dumps go
2. Find the UID and GID of the DSM user that should own the backup files.
   Over SSH: `id yourname` (typically `uid=1026 gid=100`).
3. Open **Container Manager → Project → Create**, name it `mariadb-backup`,
   choose *Create docker-compose.yml*, and paste [docker-compose.yml](docker-compose.yml).
   Set `user:` to the UID:GID from step 2, and `TZ` to your timezone.
4. Start the project. Open `http://<nas-ip>:8080` and create the admin account.
5. Recommended: **Control Panel → Login Portal → Advanced → Reverse Proxy**,
   add an HTTPS rule pointing to `localhost:8080`. The app marks its session
   cookie `Secure` automatically when it sees HTTPS via the proxy.

To update, pull the new image in Container Manager and restart the project.
Both volumes persist. If you lose the `data` volume you lose the stored
passwords (they are unrecoverable without the key file), but not the backups.

## Database user: grants required

Create a dedicated read-only user on each MariaDB server. The app never needs
anything more than this, and will refuse nothing less obviously. **Do not**
grant `ALL PRIVILEGES`.

```sql
CREATE USER 'backup'@'%' IDENTIFIED BY 'change-me';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, PROCESS, LOCK TABLES ON *.* TO 'backup'@'%';
FLUSH PRIVILEGES;
```

Restrict `'%'` to the NAS's IP address if you can. `LOCK TABLES` is listed
because `mariadb-dump` checks for it even though the app runs with
`--single-transaction` and `--skip-lock-tables`, so no locks are actually
taken. `PROCESS` is needed for some server versions to read table metadata.

## How backups are stored

```
/backups/
  <connection-slug>/
    shop_2026-10-28_020000.sql
    shop_2026-10-27_020000.sql
    ...
```

The slug is derived from the connection's display name when it is created and
never changes afterwards, so renaming a connection does not move its files.
Deleting a connection in the UI leaves its files in place.

Dumps are written to a `.partial` file and renamed on success. A failed run
never touches existing backups and never triggers pruning.

### Filename rules

```
[timestamp_][prefix][base][suffix][_timestamp].sql[.gz]
```

Timestamp format tokens: `YYYY MM DD HH mm ss`, separators `- _ . T`.
Default `YYYY-MM-DD_HHmmss`. A timestamp is required unless you enable
*overwrite mode*, which keeps exactly one file per connection.

## Restoring a backup

Restores are deliberately manual. From any machine with the MariaDB client:

```sh
# plain
mariadb -h db.example.com -u admin -p shop < shop_2026-10-28_020000.sql

# gzipped
gunzip -c shop_2026-10-28_020000.sql.gz | mariadb -h db.example.com -u admin -p shop
```

The dump contains tables, data, views, triggers, routines and events for the
one database. It does not contain `CREATE DATABASE`, so create the target
database first if it does not exist.

## Configuration

Environment variables (all optional in Docker):

| Variable       | Default        | Meaning                                  |
| -------------- | -------------- | ---------------------------------------- |
| `DATA_DIR`     | `/data`        | SQLite database, key file, temp files    |
| `BACKUP_DIR`   | `/backups`     | Backup root. Must exist and be writable  |
| `LISTEN`       | `:8080`        | HTTP listen address                      |
| `TZ`           | UTC            | Timezone for schedules and file names    |
| `LOG_LEVEL`    | `info`         | `debug`, `info`, `warn`, `error`         |
| `MARIADB_DUMP` | `mariadb-dump` | Dump binary                              |

The app refuses to start if `BACKUP_DIR` is missing or not writable.

## Security notes

- Single admin account, bcrypt-hashed, with login rate limiting.
- Session cookie is `HttpOnly`, `SameSite=Strict`, and `Secure` over HTTPS.
  State-changing requests also require a same-origin `Origin`/`Sec-Fetch-Site`.
- Strict Content-Security-Policy; no third-party scripts or styles.
- The container runs as a non-root user with a read-only root filesystem and
  all capabilities dropped.
- The "Use TLS" option encrypts the connection to MariaDB but does not verify
  the server certificate (most self-hosted servers use self-signed certs).

## Development

```sh
go test ./...
go build ./cmd/server
DATA_DIR=./data BACKUP_DIR=./backups LISTEN=:8080 ./server
```

Integration tests for the dumper run only when `MARIADB_TEST_HOST`,
`MARIADB_TEST_DB`, `MARIADB_TEST_USER` and `MARIADB_TEST_PASSWORD` are set
(see [.github/workflows/ci.yml](.github/workflows/ci.yml)). The CI user has
only the read-only grants above; a test that needs more is a bug.

Build the image locally:

```sh
docker build -t mariadb-backup .
```
