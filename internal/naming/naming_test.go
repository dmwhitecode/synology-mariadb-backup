package naming

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 28, 14, 5, 9, 0, time.UTC)

func TestBuildDefault(t *testing.T) {
	got, err := Default().Build("shop", at)
	if err != nil {
		t.Fatal(err)
	}
	if got != "shop_2026-10-28_140509.sql" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildAllParts(t *testing.T) {
	o := Options{
		UseDBName:       false,
		Base:            "custom",
		Prefix:          "pre-",
		Suffix:          "-suf",
		TimestampPrefix: true,
		TimestampSuffix: true,
		TimestampFormat: "YYYYMMDD",
		Gzip:            true,
	}
	got, err := o.Build("ignored", at)
	if err != nil {
		t.Fatal(err)
	}
	if got != "20261028_pre-custom-suf_20261028.sql.gz" {
		t.Fatalf("got %q", got)
	}
}

func TestOverwriteModeAllowsNoTimestamp(t *testing.T) {
	o := Options{UseDBName: true}
	if _, err := o.Build("shop", at); err == nil {
		t.Fatal("expected error without timestamp or overwrite")
	}
	o.Overwrite = true
	got, err := o.Build("shop", at)
	if err != nil || got != "shop.sql" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLayout(t *testing.T) {
	good := map[string]string{
		"YYYY-MM-DD_HHmmss": "2006-01-02_150405",
		"YYYYMMDDTHHmm":     "20060102T1504",
		"DD.MM.YYYY":        "02.01.2006",
	}
	for in, want := range good {
		got, err := Layout(in)
		if err != nil || got != want {
			t.Errorf("Layout(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"YYYY/MM", "2006", "YYYY MM", "YY", "HHmmss.000", "../YYYY"} {
		if _, err := Layout(bad); err == nil {
			t.Errorf("Layout(%q) accepted", bad)
		}
	}
	if got, _ := Layout(""); got != "2006-01-02_150405" {
		t.Errorf("empty format should use default, got %q", got)
	}
}

func TestSanitise(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":    "etcpasswd",
		"..":                  "",
		"a/b\\c":              "abc",
		".hidden":             "hidden",
		"nul\x00byte":         "nulbyte",
		"tab\tnew\nline":      "tabnewline",
		`what:is*this?"<>|`:   "whatisthis",
		"  spaced  ":          "spaced",
		"ok-name_1.2":         "ok-name_1.2",
		"a....b":              "a.b",
	}
	for in, want := range cases {
		if got := Sanitise(in); got != want {
			t.Errorf("Sanitise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRejectsTraversal(t *testing.T) {
	o := Default()
	o.Prefix = "../../"
	got, err := o.Build("../x", at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
		t.Fatalf("unsafe name %q", got)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Production Shop DB": "production-shop-db",
		"  weird__name!!":    "weird-name",
		"///":                "connection",
		"Ünïcode café":       "n-code-caf",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainedPath(t *testing.T) {
	root := t.TempDir()
	p, err := ContainedPath(root, "conn", "file.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, root) || filepath.Base(p) != "file.sql" {
		t.Fatalf("unexpected %q", p)
	}
	for _, parts := range [][]string{
		{"..", "x"}, {"conn", ".."}, {"a/b", "c"}, {`a\b`}, {""}, {"."}, {filepath.Join(root, "..")},
	} {
		if _, err := ContainedPath(root, parts...); err == nil {
			t.Errorf("ContainedPath(%v) should fail", parts)
		}
	}
}
