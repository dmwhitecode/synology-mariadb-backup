// Package naming builds backup filenames from user options and makes sure
// the result can never escape the backup root.
//
// Shape:
//
//	[timestamp_][prefix][base][suffix][_timestamp].sql[.gz]
package naming

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// DefaultTimestampFormat is the token format used when none is configured.
const DefaultTimestampFormat = "YYYY-MM-DD_HHmmss"

// Options describes how to name a backup file.
type Options struct {
	// UseDBName makes the database name the base of the filename.
	UseDBName bool `json:"use_db_name"`
	// Base is used instead of the database name when UseDBName is false.
	Base string `json:"base,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// TimestampPrefix / TimestampSuffix place a timestamp at the start / end.
	TimestampPrefix bool `json:"timestamp_prefix"`
	TimestampSuffix bool `json:"timestamp_suffix"`
	// TimestampFormat uses the tokens YYYY MM DD HH mm ss and the separators - _ . T
	TimestampFormat string `json:"timestamp_format,omitempty"`
	// Gzip compresses the dump and appends .gz.
	Gzip bool `json:"gzip"`
	// Overwrite allows a name with no timestamp. Each run then replaces the
	// previous file, so retention cannot keep multiple versions.
	Overwrite bool `json:"overwrite"`
}

// Default returns the naming used when a user has not configured anything.
func Default() Options {
	return Options{
		UseDBName:       true,
		TimestampSuffix: true,
		TimestampFormat: DefaultTimestampFormat,
	}
}

var formatRe = regexp.MustCompile(`^(YYYY|MM|DD|HH|mm|ss|[-_.T])+$`)

// tokenLayout maps the user-facing tokens to Go's reference-time layout.
// Order matters: longer / case-sensitive tokens first.
var tokenLayout = []struct{ token, layout string }{
	{"YYYY", "2006"},
	{"DD", "02"},
	{"HH", "15"},
	{"MM", "01"},
	{"mm", "04"},
	{"ss", "05"},
}

// Validate checks the options without needing a database name.
func (o Options) Validate() error {
	if !o.UseDBName && Sanitise(o.Base) == "" {
		return errors.New("a base name is required when not using the database name")
	}
	if !o.TimestampPrefix && !o.TimestampSuffix && !o.Overwrite {
		return errors.New("add a timestamp prefix or suffix, or enable overwrite mode")
	}
	if o.TimestampPrefix || o.TimestampSuffix {
		if _, err := Layout(o.TimestampFormat); err != nil {
			return err
		}
	}
	return nil
}

// Layout converts a token format into a Go time layout.
func Layout(format string) (string, error) {
	format = strings.TrimSpace(format)
	if format == "" {
		format = DefaultTimestampFormat
	}
	if !formatRe.MatchString(format) {
		return "", fmt.Errorf("timestamp format %q may only contain YYYY MM DD HH mm ss and - _ . T", format)
	}
	layout := format
	for _, tl := range tokenLayout {
		layout = strings.ReplaceAll(layout, tl.token, tl.layout)
	}
	return layout, nil
}

// Build returns the filename (no directory) for a dump of database taken at t.
func (o Options) Build(database string, t time.Time) (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	base := Sanitise(database)
	if !o.UseDBName {
		base = Sanitise(o.Base)
	}
	if base == "" {
		return "", errors.New("filename base is empty after sanitising")
	}

	var ts string
	if o.TimestampPrefix || o.TimestampSuffix {
		layout, err := Layout(o.TimestampFormat)
		if err != nil {
			return "", err
		}
		ts = t.Format(layout)
	}

	var sb strings.Builder
	if o.TimestampPrefix {
		sb.WriteString(ts)
		sb.WriteString("_")
	}
	sb.WriteString(Sanitise(o.Prefix))
	sb.WriteString(base)
	sb.WriteString(Sanitise(o.Suffix))
	if o.TimestampSuffix {
		sb.WriteString("_")
		sb.WriteString(ts)
	}
	sb.WriteString(".sql")
	if o.Gzip {
		sb.WriteString(".gz")
	}

	name := Sanitise(sb.String())
	if name == "" || name == ".sql" || name == ".sql.gz" {
		return "", errors.New("filename is empty after sanitising")
	}
	return name, nil
}

// Sanitise strips anything from a filename part that could change the path
// it resolves to: separators, "..", control characters, reserved characters
// and leading dots or whitespace.
func Sanitise(part string) string {
	var sb strings.Builder
	for _, r := range part {
		switch {
		case r == '/' || r == '\\' || r == 0:
			continue
		case unicode.IsControl(r):
			continue
		case strings.ContainsRune(`:*?"<>|`, r):
			continue
		}
		sb.WriteRune(r)
	}
	s := sb.String()
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	s = strings.TrimLeft(s, ". \t")
	s = strings.TrimRight(s, " \t")
	return s
}

// Slug turns a display name into a safe directory name.
func Slug(name string) string {
	var sb strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			sb.WriteRune('-')
			lastDash = true
		}
	}
	s := strings.Trim(sb.String(), "-")
	if s == "" {
		s = "connection"
	}
	return s
}

// ContainedPath joins root with the given parts and guarantees the result is
// inside root. It returns an error if any part would escape.
func ContainedPath(root string, parts ...string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) || filepath.IsAbs(p) {
			return "", fmt.Errorf("invalid path component %q", p)
		}
	}
	full := filepath.Join(append([]string{absRoot}, parts...)...)
	rel, err := filepath.Rel(absRoot, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes backup root", full)
	}
	return full, nil
}
