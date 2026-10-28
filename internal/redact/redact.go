// Package redact scrubs credentials out of strings before they are logged or
// shown to a user.
package redact

import (
	"regexp"
	"strings"
)

const mask = "***"

// patterns catch the common ways a password can leak into client output:
// option-file style (password=...), URL userinfo (user:pass@host) and the
// mariadb -p<password> flag.
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(password|passwd|pwd)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`),
	regexp.MustCompile(`(?i)(--password|-p)(=|\s+)?("[^"]*"|'[^']*'|\S+)`),
	regexp.MustCompile(`([a-zA-Z0-9_.-]+):([^@\s/]+)@`),
}

// Scrub replaces every occurrence of the given secrets and any password-like
// pattern in s with a mask. Empty secrets are ignored.
func Scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		s = strings.ReplaceAll(s, sec, mask)
	}
	s = patterns[0].ReplaceAllString(s, "${1}${2}"+mask)
	s = patterns[1].ReplaceAllString(s, "${1}${2}"+mask)
	s = patterns[2].ReplaceAllString(s, "${1}:"+mask+"@")
	return s
}
