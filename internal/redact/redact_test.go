package redact

import (
	"strings"
	"testing"
)

func TestScrubKnownSecret(t *testing.T) {
	in := "ERROR 1045: Access denied for user 'bob'@'10.0.0.5' (using password: s3cr3t!)"
	out := Scrub(in, "s3cr3t!")
	if strings.Contains(out, "s3cr3t!") {
		t.Fatalf("secret leaked: %s", out)
	}
}

func TestScrubPatterns(t *testing.T) {
	cases := map[string]string{
		`password="abc"`:                    `abc`,
		`password=abc def`:                  `abc`,
		`PASSWORD: xyz`:                     `xyz`,
		`mariadb-dump -pqwerty db`:          `qwerty`,
		`--password=hunter2 --host=x`:       `hunter2`,
		`mysql://bob:topsecret@db.local/x`:  `topsecret`,
		`user:p@ss@host`:                    `p@ss`,
	}
	for in, secret := range cases {
		out := Scrub(in)
		if strings.Contains(out, secret) {
			t.Errorf("Scrub(%q) = %q, still contains %q", in, out, secret)
		}
	}
}

func TestScrubLeavesHarmlessText(t *testing.T) {
	in := "Got error: 2003 Can't connect to server on 'db.local' (111)"
	if out := Scrub(in, ""); out != in {
		t.Fatalf("harmless text changed: %q", out)
	}
}
