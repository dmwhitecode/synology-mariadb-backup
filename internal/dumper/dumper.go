// Package dumper runs read-only probes against a MariaDB server and produces
// SQL dumps by shelling out to mariadb-dump.
//
// Nothing in this package writes to the target server. The only statements
// issued are SET SESSION TRANSACTION READ ONLY, SELECT VERSION() and
// SHOW DATABASES. Dumps are produced by mariadb-dump with
// --single-transaction and without table locks.
package dumper

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/redact"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/retention"
)

// Target is everything needed to reach one database. Password is plaintext
// and must only exist in memory for the duration of a call.
type Target struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	TLS      bool
}

// Validate checks the target fields that end up in a command line or option
// file, so a malicious value cannot become an extra option.
func (t Target) Validate() error {
	if strings.TrimSpace(t.Host) == "" {
		return errors.New("host is required")
	}
	if t.Port < 1 || t.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if strings.TrimSpace(t.Database) == "" {
		return errors.New("database is required")
	}
	if strings.HasPrefix(t.Database, "-") || strings.ContainsAny(t.Database, "\x00\n\r/\\") {
		return errors.New("database name contains invalid characters")
	}
	if strings.TrimSpace(t.Username) == "" {
		return errors.New("username is required")
	}
	if strings.ContainsAny(t.Host+t.Username, "\x00\n\r") {
		return errors.New("host or username contains invalid characters")
	}
	if strings.ContainsAny(t.Password, "\x00\n\r") {
		return errors.New("password contains invalid characters")
	}
	return nil
}

// TestResult is what a connection probe reports back to the UI.
type TestResult struct {
	Version         string `json:"version"`
	DatabaseVisible bool   `json:"database_visible"`
	Latency         string `json:"latency"`
}

// TestConnection opens a read-only session, reads the server version and
// checks that the configured database is visible to the user.
func TestConnection(ctx context.Context, t Target) (TestResult, error) {
	var res TestResult
	if err := t.Validate(); err != nil {
		return res, err
	}
	cfg := mysql.NewConfig()
	cfg.User = t.Username
	cfg.Passwd = t.Password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", t.Host, t.Port)
	cfg.Timeout = 8 * time.Second
	cfg.ReadTimeout = 15 * time.Second
	cfg.WriteTimeout = 15 * time.Second
	if t.TLS {
		cfg.TLSConfig = "skip-verify"
	}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return res, scrub(err, t.Password)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	start := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return res, scrub(err, t.Password)
	}
	defer conn.Close()

	// Belt-and-braces: even if a later change accidentally issued a write,
	// the server would refuse it for this session.
	if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return res, scrub(err, t.Password)
	}
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&res.Version); err != nil {
		return res, scrub(err, t.Password)
	}
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return res, scrub(err, t.Password)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return res, scrub(err, t.Password)
		}
		if strings.EqualFold(name, t.Database) {
			res.DatabaseVisible = true
		}
	}
	if err := rows.Err(); err != nil {
		return res, scrub(err, t.Password)
	}
	res.Latency = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

// Dumper shells out to mariadb-dump.
type Dumper struct {
	// Binary is the mariadb-dump executable (name on PATH or full path).
	Binary string
	// TempDir is where the 0600 option file is written. Must be private.
	TempDir string
	// Timeout bounds a single dump. Zero means no limit.
	Timeout time.Duration
}

// Dump writes a dump of t.Database to dest. It writes to a .partial file
// first and renames on success, so a failed run never clobbers a good file.
// It returns the size of the finished file.
func (d *Dumper) Dump(ctx context.Context, t Target, dest string, gz bool) (int64, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return 0, err
	}

	optFile, err := writeOptionFile(d.TempDir, t)
	if err != nil {
		return 0, err
	}
	defer os.Remove(optFile)

	partial := dest + retention.PartialSuffix
	out, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	success := false
	defer func() {
		if !success {
			out.Close()
			os.Remove(partial)
		}
	}()

	var w io.Writer = out
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(out)
		w = gzw
	}

	cmd := exec.CommandContext(ctx, d.Binary, Args(optFile, t.Database)...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 64 * 1024}
	cmd.Env = minimalEnv()

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			msg = "dump timed out or was cancelled: " + msg
		}
		return 0, errors.New(redact.Scrub(msg, t.Password))
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			return 0, err
		}
	}
	if err := out.Sync(); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(partial, dest); err != nil {
		return 0, err
	}
	success = true
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Args returns the mariadb-dump arguments. Exposed for tests so the exact
// flags (and the absence of any locking or write-capable ones) are pinned.
func Args(optFile, database string) []string {
	return []string{
		"--defaults-extra-file=" + optFile, // must be first
		"--single-transaction",
		"--skip-lock-tables",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--no-tablespaces",
		"--hex-blob",
		"--skip-comments",
		database,
	}
}

// writeOptionFile writes a [client] section with the credentials to a
// private temp file so the password never appears on the command line.
func writeOptionFile(dir string, t Target) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "dump-*.cnf")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0o600); err != nil && !isWindows() {
		f.Close()
		os.Remove(name)
		return "", err
	}
	var b strings.Builder
	b.WriteString("[client]\n")
	b.WriteString("host=" + quoteOpt(t.Host) + "\n")
	b.WriteString("port=" + strconv.Itoa(t.Port) + "\n")
	b.WriteString("user=" + quoteOpt(t.Username) + "\n")
	b.WriteString("password=" + quoteOpt(t.Password) + "\n")
	if t.TLS {
		b.WriteString("ssl\n")
		b.WriteString("skip-ssl-verify-server-cert\n")
	} else {
		b.WriteString("skip-ssl\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// quoteOpt quotes a value for a MariaDB option file. Backslashes and double
// quotes are escaped; newlines were rejected by Validate.
func quoteOpt(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// minimalEnv hands the child only what it needs. In particular it never
// receives MYSQL_PWD or the app's own environment.
func minimalEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "TZ", "LANG", "LC_ALL", "SystemRoot", "TEMP", "TMP"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func scrub(err error, secret string) error {
	return errors.New(redact.Scrub(err.Error(), secret))
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.w.Len() < l.n {
		room := l.n - l.w.Len()
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}
