package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAdminAndSessions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if has, _ := s.HasAdmin(ctx); has {
		t.Fatal("fresh db should have no admin")
	}
	if err := s.CreateAdmin(ctx, "admin", "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdmin(ctx, "other", "hash2"); err == nil {
		t.Fatal("second admin must be rejected")
	}
	u, h, err := s.GetAdmin(ctx)
	if err != nil || u != "admin" || h != "hash1" {
		t.Fatalf("got %q %q %v", u, h, err)
	}
	if err := s.CreateSession(ctx, "tok", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.SessionValid(ctx, "tok"); !ok {
		t.Fatal("session should be valid")
	}
	if ok, _ := s.SessionValid(ctx, "nope"); ok {
		t.Fatal("unknown session valid")
	}
	s.CreateSession(ctx, "old", time.Now().Add(-time.Minute))
	if ok, _ := s.SessionValid(ctx, "old"); ok {
		t.Fatal("expired session valid")
	}
	s.DeleteAllSessions(ctx)
	if ok, _ := s.SessionValid(ctx, "tok"); ok {
		t.Fatal("session survived DeleteAllSessions")
	}
}

func TestDefaultsRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	d, err := s.GetDefaults(ctx)
	if err != nil || d.RetentionKeep != 7 || d.Schedule.Mode != schedule.Daily {
		t.Fatalf("unexpected initial defaults %+v %v", d, err)
	}
	d.RetentionKeep = 3
	d.Naming.Gzip = true
	d.Schedule = schedule.Schedule{Mode: schedule.Weekly, Time: "01:00", Days: []int{1, 3}}
	if err := s.SaveDefaults(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDefaults(ctx)
	if got.RetentionKeep != 3 || !got.Naming.Gzip || got.Schedule.Mode != schedule.Weekly || len(got.Schedule.Days) != 2 {
		t.Fatalf("defaults not persisted: %+v", got)
	}
}

func TestConnectionCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	keep := 3
	c := &model.Connection{
		Name: "Prod Shop", Host: "db.example", Port: 3307, Database: "shop",
		Username: "ro", PasswordEnc: "v1:abc", TLS: true, Enabled: true,
		RetentionKeep: &keep,
		Naming:        &naming.Options{UseDBName: true, TimestampPrefix: true, TimestampFormat: "YYYYMMDD"},
	}
	if err := s.CreateConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 || c.Slug != "prod-shop" {
		t.Fatalf("id=%d slug=%q", c.ID, c.Slug)
	}
	// Same name gets a distinct slug.
	c2 := &model.Connection{Name: "Prod Shop", Host: "h", Port: 3306, Database: "d", Username: "u", PasswordEnc: "v1:x", Enabled: true}
	s.CreateConnection(ctx, c2)
	if c2.Slug != "prod-shop-2" {
		t.Fatalf("slug collision not handled: %q", c2.Slug)
	}

	got, err := s.GetConnection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "db.example" || got.Port != 3307 || !got.TLS || got.Schedule != nil ||
		got.RetentionKeep == nil || *got.RetentionKeep != 3 || got.Naming == nil || !got.Naming.TimestampPrefix {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// Update with empty password keeps the old one; slug never changes.
	got.Name = "Renamed"
	got.PasswordEnc = ""
	got.RetentionKeep = nil
	if err := s.UpdateConnection(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetConnection(ctx, c.ID)
	if again.Name != "Renamed" || again.PasswordEnc != "v1:abc" || again.Slug != "prod-shop" || again.RetentionKeep != nil {
		t.Fatalf("update wrong: %+v", again)
	}
	again.PasswordEnc = "v1:new"
	s.UpdateConnection(ctx, again)
	again, _ = s.GetConnection(ctx, c.ID)
	if again.PasswordEnc != "v1:new" {
		t.Fatal("password not updated")
	}

	s.SetConnectionEnabled(ctx, c.ID, false)
	again, _ = s.GetConnection(ctx, c.ID)
	if again.Enabled {
		t.Fatal("still enabled")
	}

	list, _ := s.ListConnections(ctx)
	if len(list) != 2 {
		t.Fatalf("list len %d", len(list))
	}

	if err := s.DeleteConnection(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConnection(ctx, c.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := s.UpdateConnection(ctx, c); err != ErrNotFound {
		t.Fatalf("update of missing row should be ErrNotFound, got %v", err)
	}
}

func TestRuns(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	c := &model.Connection{Name: "A", Host: "h", Port: 3306, Database: "d", Username: "u", PasswordEnc: "x", Enabled: true}
	s.CreateConnection(ctx, c)

	id, err := s.CreateRun(ctx, c.ID, model.TriggerManual, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MarkInterruptedRuns(ctx); n != 1 {
		t.Fatalf("interrupted = %d", n)
	}
	id2, _ := s.CreateRun(ctx, c.ID, model.TriggerScheduled, time.Now())
	s.FinishRun(ctx, id2, model.StatusSuccess, "a/file.sql", 1234, "")
	s.RecordSkipped(ctx, c.ID, "busy")

	runs, _ := s.ListRuns(ctx, c.ID, 10)
	if len(runs) != 3 || runs[0].Status != model.StatusSkipped || runs[1].ID != id2 || runs[1].Size != 1234 || runs[2].ID != id || runs[2].Status != model.StatusFailed {
		t.Fatalf("runs wrong: %+v", runs)
	}
	last, _ := s.LastRuns(ctx)
	if last[c.ID] == nil || last[c.ID].Status != model.StatusSkipped {
		t.Fatalf("last runs wrong: %+v", last)
	}
	s.PruneRuns(ctx, c.ID, 1)
	runs, _ = s.ListRuns(ctx, c.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("prune kept %d", len(runs))
	}

	// Deleting the connection cascades to runs.
	s.DeleteConnection(ctx, c.ID)
	runs, _ = s.ListRuns(ctx, c.ID, 10)
	if len(runs) != 0 {
		t.Fatal("runs not cascaded")
	}
}
