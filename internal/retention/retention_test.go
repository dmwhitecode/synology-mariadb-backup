package retention

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func touch(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func names(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	sort.Strings(out)
	return out
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "a_1.sql", 5*time.Hour)
	touch(t, dir, "a_2.sql", 4*time.Hour)
	touch(t, dir, "a_3.sql.gz", 3*time.Hour)
	newest := touch(t, dir, "a_4.sql", 0)
	touch(t, dir, "notes.txt", 10*time.Hour)      // not a dump: ignored
	touch(t, dir, "a_0.sql.partial", 10*time.Hour) // in-progress marker: ignored
	os.Mkdir(filepath.Join(dir, "sub.sql"), 0o750) // directory: ignored

	deleted, err := Prune(dir, 2, newest)
	if err != nil {
		t.Fatal(err)
	}
	got := names(deleted)
	if len(got) != 2 || got[0] != "a_1.sql" || got[1] != "a_2.sql" {
		t.Fatalf("deleted %v", got)
	}
	for _, keep := range []string{"a_3.sql.gz", "a_4.sql", "notes.txt", "a_0.sql.partial"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s should still exist", keep)
		}
	}
}

func TestPruneNeverDeletesProtected(t *testing.T) {
	dir := t.TempDir()
	// Protected file has an old mtime (e.g. clock skew) but must survive.
	protected := touch(t, dir, "old_but_mine.sql", 48*time.Hour)
	touch(t, dir, "b.sql", 1*time.Hour)
	touch(t, dir, "c.sql", 0)

	deleted, err := Prune(dir, 1, protected)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(deleted); len(got) != 1 || got[0] != "b.sql" {
		t.Fatalf("deleted %v", got)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatal("protected file was deleted")
	}
}

func TestPruneRejectsZeroKeep(t *testing.T) {
	if _, err := Prune(t.TempDir(), 0, ""); err == nil {
		t.Fatal("keep=0 would delete everything; must be rejected")
	}
}

func TestCleanStalePartials(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "fresh.sql.partial", time.Hour)
	touch(t, dir, "stale.sql.partial", 48*time.Hour)
	touch(t, dir, "done.sql", 48*time.Hour)
	removed, err := CleanStalePartials(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(removed); len(got) != 1 || got[0] != "stale.sql.partial" {
		t.Fatalf("removed %v", got)
	}
}
