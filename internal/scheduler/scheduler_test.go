package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/dumper"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/vault"
)

// fakeDump is a tiny script standing in for mariadb-dump: it sleeps briefly
// then prints a dump to stdout, so the scheduler's file handling and overlap
// logic can be exercised without a database.
func fakeDump(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		p := filepath.Join(t.TempDir(), "fakedump.cmd")
		os.WriteFile(p, []byte("@echo off\r\nping -n 2 127.0.0.1 >nul\r\necho -- fake dump\r\n"), 0o700)
		return p
	}
	p := filepath.Join(t.TempDir(), "fakedump.sh")
	os.WriteFile(p, []byte("#!/bin/sh\nsleep 1\necho '-- fake dump'\n"), 0o700)
	return p
}

func newTestScheduler(t *testing.T) (*Scheduler, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, _ := vault.Open(filepath.Join(dir, "k"))
	root := filepath.Join(dir, "backups")
	os.MkdirAll(root, 0o750)
	d := &dumper.Dumper{Binary: fakeDump(t), TempDir: dir}
	s := New(st, v, d, root, slog.New(slog.NewTextHandler(io.Discard, nil)))

	enc, _ := v.Encrypt("pw")
	keep := 2
	c := &model.Connection{
		Name: "A", Host: "h", Port: 3306, Database: "db", Username: "u", PasswordEnc: enc, Enabled: true,
		RetentionKeep: &keep,
		Schedule:      &schedule.Schedule{Mode: schedule.Daily, Time: "03:00"},
	}
	if err := st.CreateConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return s, st, root
}

func TestRunNowWritesFileAndPrunes(t *testing.T) {
	s, st, root := newTestScheduler(t)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(ctx)

	for i := 0; i < 3; i++ {
		if err := s.RunNow(ctx, 1, model.TriggerManual); err != nil {
			t.Fatal(err)
		}
		// Distinct timestamps so filenames differ.
		time.Sleep(1100 * time.Millisecond)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "a"))
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("retention keep=2 but found %v", names)
	}
	runs, _ := st.ListRuns(ctx, 1, 10)
	if len(runs) != 3 || runs[0].Status != model.StatusSuccess || runs[0].Size == 0 || runs[0].File == "" {
		t.Fatalf("runs: %+v", runs[0])
	}
	if next := s.NextRun(1); next.IsZero() {
		t.Fatal("enabled connection should have a next run")
	}
}

func TestOverlapIsRejected(t *testing.T) {
	s, st, _ := newTestScheduler(t)
	ctx := context.Background()

	if err := s.RunAsync(1, model.TriggerManual); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !s.IsRunning(1) {
		t.Fatal("expected running")
	}
	if err := s.RunNow(ctx, 1, model.TriggerManual); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
	if err := s.RunAsync(1, model.TriggerManual); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("expected ErrAlreadyRunning, got %v", err)
	}
	// A scheduled fire during a run is recorded as skipped, not queued.
	s.scheduledRun(1)
	runs, _ := st.ListRuns(ctx, 1, 10)
	if len(runs) != 2 || runs[0].Status != model.StatusSkipped {
		t.Fatalf("expected skipped row, got %+v", runs)
	}

	s.Stop(ctx) // waits for the in-flight run
	if s.IsRunning(1) {
		t.Fatal("still running after Stop")
	}
}

func TestDisabledConnectionNotScheduled(t *testing.T) {
	s, st, _ := newTestScheduler(t)
	ctx := context.Background()
	st.SetConnectionEnabled(ctx, 1, false)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(ctx)
	if !s.NextRun(1).IsZero() {
		t.Fatal("disabled connection has a next run")
	}
	// Manual runs still work when disabled; scheduled ones do not.
	if err := s.RunNow(ctx, 1, model.TriggerScheduled); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if err := s.RunNow(ctx, 1, model.TriggerManual); err != nil {
		t.Fatalf("manual run on disabled connection: %v", err)
	}
}
