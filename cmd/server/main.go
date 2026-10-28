// Command server is the MariaDB backup app: scheduler plus admin web UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/config"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/dumper"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/scheduler"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/vault"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	log := newLogger(cfg.LogLevel)
	log.Info("starting", "version", version, "data_dir", cfg.DataDir, "backup_dir", cfg.BackupDir, "listen", cfg.Listen, "tz", time.Local.String())

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir %s: %w", cfg.DataDir, err)
	}
	if err := checkWritable(cfg.BackupDir); err != nil {
		return fmt.Errorf("backup root %s is not usable: %w (mount a writable volume there)", cfg.BackupDir, err)
	}
	checkDumpBinary(log, cfg.DumpBinary)

	st, err := store.Open(filepath.Join(cfg.DataDir, "app.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	v, err := vault.Open(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return err
	}

	ctx := context.Background()
	if n, err := st.MarkInterruptedRuns(ctx); err != nil {
		log.Error("could not mark interrupted runs", "err", err)
	} else if n > 0 {
		log.Warn("marked runs interrupted by previous shutdown", "count", n)
	}
	_ = st.PurgeExpiredSessions(ctx)

	d := &dumper.Dumper{
		Binary:  cfg.DumpBinary,
		TempDir: filepath.Join(cfg.DataDir, "tmp"),
		Timeout: 12 * time.Hour,
	}
	sched := scheduler.New(st, v, d, cfg.BackupDir, log)
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}

	srv, err := web.New(st, v, sched, cfg.BackupDir, cfg.DataDir, version, log)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("web ui listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	sched.Stop(shutdownCtx)
	log.Info("stopped")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// checkWritable fails loudly if the backup root is missing or read-only.
func checkWritable(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func checkDumpBinary(log *slog.Logger, bin string) {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		log.Error("mariadb-dump not found or not runnable; backups will fail until this is fixed", "binary", bin, "err", err)
		return
	}
	log.Info("dump tool", "version", strings.TrimSpace(string(out)))
}
