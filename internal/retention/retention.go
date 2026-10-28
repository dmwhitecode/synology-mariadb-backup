// Package retention deletes old backup files so that only the newest N
// remain.
package retention

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PartialSuffix marks a dump that is still being written.
const PartialSuffix = ".partial"

// StalePartialAge is how old a .partial file must be before it is treated as
// an abandoned write and removed.
const StalePartialAge = 24 * time.Hour

type entry struct {
	path string
	mod  time.Time
}

// Prune keeps the newest `keep` dump files in dir and deletes the rest. The
// file at protect is never deleted, whatever its age. It returns the paths it
// removed. Only files ending in .sql or .sql.gz are considered.
func Prune(dir string, keep int, protect string) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("retention: keep must be at least 1")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	protectAbs, _ := filepath.Abs(protect)

	var files []entry
	for _, e := range entries {
		if !e.Type().IsRegular() || !IsDump(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, entry{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].path > files[j].path
	})

	var deleted []string
	for i, f := range files {
		if i < keep {
			continue
		}
		if abs, _ := filepath.Abs(f.path); abs == protectAbs {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return deleted, err
		}
		deleted = append(deleted, f.path)
	}
	return deleted, nil
}

// CleanStalePartials removes abandoned .partial files older than
// StalePartialAge. It never touches finished dumps.
func CleanStalePartials(dir string, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), PartialSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < StalePartialAge {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed, nil
}

// IsDump reports whether name looks like a finished dump file.
func IsDump(name string) bool {
	return strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".sql.gz")
}
