// Package scheduler runs backups on their cron schedules and on demand,
// guaranteeing at most one run per connection at a time.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/dumper"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/redact"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/retention"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/vault"
)

// ErrAlreadyRunning is returned by RunNow when a backup for the connection
// is in progress.
var ErrAlreadyRunning = errors.New("a backup for this connection is already running")

// ErrDisabled is returned when a scheduled run fires for a disabled
// connection (should not happen, but is guarded).
var ErrDisabled = errors.New("connection is disabled")

// runHistoryKeep is how many run rows to keep per connection.
const runHistoryKeep = 200

// Scheduler owns the cron instance and the per-connection run locks.
type Scheduler struct {
	store      *store.Store
	vault      *vault.Vault
	dumper     *dumper.Dumper
	backupRoot string
	log        *slog.Logger

	cron    *cron.Cron
	mu      sync.Mutex
	entries map[int64]cron.EntryID // connection id -> cron entry
	running map[int64]bool         // connection id -> in progress
	wg      sync.WaitGroup
}

// New creates a scheduler. Call Start to begin firing schedules.
func New(st *store.Store, v *vault.Vault, d *dumper.Dumper, backupRoot string, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store:      st,
		vault:      v,
		dumper:     d,
		backupRoot: backupRoot,
		log:        log,
		cron:       cron.New(cron.WithLocation(time.Local), cron.WithParser(schedule.Parser)),
		entries:    map[int64]cron.EntryID{},
		running:    map[int64]bool{},
	}
}

// Start loads schedules and starts the cron loop.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		return err
	}
	s.cron.Start()
	return nil
}

// Stop halts the cron loop and waits for in-flight backups to finish or
// the context to expire.
func (s *Scheduler) Stop(ctx context.Context) {
	stopCtx := s.cron.Stop()
	done := make(chan struct{})
	go func() {
		<-stopCtx.Done()
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Reload rebuilds the cron entries from the store. Call after any change to
// a connection or to the global defaults.
func (s *Scheduler) Reload(ctx context.Context) error {
	conns, err := s.store.ListConnections(ctx)
	if err != nil {
		return err
	}
	defaults, err := s.store.GetDefaults(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.entries {
		s.cron.Remove(id)
	}
	s.entries = map[int64]cron.EntryID{}

	for _, c := range conns {
		if !c.Enabled {
			continue
		}
		expr, err := c.Effective(defaults).Schedule.CronExpr()
		if err != nil {
			s.log.Error("invalid schedule, connection will not run", "connection", c.Name, "err", err)
			continue
		}
		id := c.ID
		entry, err := s.cron.AddFunc(expr, func() { s.scheduledRun(id) })
		if err != nil {
			s.log.Error("could not schedule", "connection", c.Name, "err", err)
			continue
		}
		s.entries[c.ID] = entry
	}
	return nil
}

// NextRun returns when the connection will next run, or zero if it is not
// scheduled.
func (s *Scheduler) NextRun(connID int64) time.Time {
	s.mu.Lock()
	id, ok := s.entries[connID]
	s.mu.Unlock()
	if !ok {
		return time.Time{}
	}
	return s.cron.Entry(id).Next
}

// IsRunning reports whether a backup is in progress for the connection.
func (s *Scheduler) IsRunning(connID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[connID]
}

func (s *Scheduler) scheduledRun(connID int64) {
	ctx := context.Background()
	err := s.RunNow(ctx, connID, model.TriggerScheduled)
	if errors.Is(err, ErrAlreadyRunning) {
		s.log.Warn("scheduled backup skipped: previous run still in progress", "connection_id", connID)
		if e := s.store.RecordSkipped(ctx, connID, "previous run still in progress"); e != nil {
			s.log.Error("could not record skipped run", "err", e)
		}
		return
	}
	if err != nil {
		s.log.Error("scheduled backup failed", "connection_id", connID, "err", err)
	}
}

// RunNow executes a backup for the connection synchronously. It returns
// ErrAlreadyRunning without doing anything if one is in progress.
func (s *Scheduler) RunNow(ctx context.Context, connID int64, trigger string) error {
	s.mu.Lock()
	if s.running[connID] {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.running[connID] = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, connID)
		s.mu.Unlock()
		s.wg.Done()
	}()
	return s.backup(ctx, connID, trigger)
}

// RunAsync starts a backup in the background and returns immediately. It
// still returns ErrAlreadyRunning synchronously if one is in progress.
func (s *Scheduler) RunAsync(connID int64, trigger string) error {
	s.mu.Lock()
	if s.running[connID] {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.running[connID] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, connID)
			s.mu.Unlock()
			s.wg.Done()
		}()
		if err := s.backup(context.Background(), connID, trigger); err != nil {
			s.log.Error("backup failed", "connection_id", connID, "err", err)
		}
	}()
	return nil
}

func (s *Scheduler) backup(ctx context.Context, connID int64, trigger string) error {
	c, err := s.store.GetConnection(ctx, connID)
	if err != nil {
		return err
	}
	if trigger == model.TriggerScheduled && !c.Enabled {
		return ErrDisabled
	}
	defaults, err := s.store.GetDefaults(ctx)
	if err != nil {
		return err
	}
	eff := c.Effective(defaults)
	log := s.log.With("connection", c.Name, "trigger", trigger)

	started := time.Now()
	runID, err := s.store.CreateRun(ctx, c.ID, trigger, started)
	if err != nil {
		return err
	}
	fail := func(msg string) error {
		msg = redact.Scrub(msg)
		log.Error("backup failed", "err", msg)
		if e := s.store.FinishRun(ctx, runID, model.StatusFailed, "", 0, msg); e != nil {
			log.Error("could not record failure", "err", e)
		}
		return errors.New(msg)
	}

	filename, err := eff.Naming.Build(c.Database, started)
	if err != nil {
		return fail("filename: " + err.Error())
	}
	dest, err := naming.ContainedPath(s.backupRoot, c.Slug, filename)
	if err != nil {
		return fail(err.Error())
	}

	password, err := s.vault.Decrypt(c.PasswordEnc)
	if err != nil {
		return fail("could not decrypt stored password: " + err.Error())
	}
	target := dumper.Target{
		Host: c.Host, Port: c.Port, Database: c.Database,
		Username: c.Username, Password: password, TLS: c.TLS,
	}

	log.Info("backup started", "file", filename)
	size, err := s.dumper.Dump(ctx, target, dest, eff.Naming.Gzip)
	password = "" //nolint:ineffassign // drop the reference as early as possible
	if err != nil {
		return fail(err.Error())
	}
	relFile := filepath.ToSlash(filepath.Join(c.Slug, filename))
	if err := s.store.FinishRun(ctx, runID, model.StatusSuccess, relFile, size, ""); err != nil {
		log.Error("could not record success", "err", err)
	}
	log.Info("backup finished", "file", relFile, "bytes", size, "duration", time.Since(started).Round(time.Millisecond))

	// Pruning only after a successful write, and never the file just written.
	if !eff.Naming.Overwrite {
		dir := filepath.Dir(dest)
		deleted, err := retention.Prune(dir, eff.RetentionKeep, dest)
		if err != nil {
			log.Error("retention pruning failed", "err", err)
		}
		for _, p := range deleted {
			log.Info("pruned old backup", "file", filepath.Base(p))
		}
		if removed, _ := retention.CleanStalePartials(dir, time.Now()); len(removed) > 0 {
			log.Warn("removed abandoned partial files", "count", len(removed))
		}
	}
	if err := s.store.PruneRuns(ctx, c.ID, runHistoryKeep); err != nil {
		log.Error("could not prune run history", "err", err)
	}
	return nil
}

// Describe is a convenience for the UI: "Daily at 02:00 (next 29 Oct 02:00)".
func (s *Scheduler) Describe(c *model.Connection, d model.Defaults) string {
	desc := c.Effective(d).Schedule.Describe()
	if next := s.NextRun(c.ID); !next.IsZero() {
		return fmt.Sprintf("%s (next %s)", desc, next.Format("02 Jan 15:04"))
	}
	return desc
}
