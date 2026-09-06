// Package envcfg reads configuration from environment variables.
//
// cmd.exe's `set VAR="value"` doesn't strip the quotes the way a POSIX
// shell would, so os.Getenv sees them literally. Every value read from
// the environment goes through Clean to strip them back off.
package envcfg

import (
	"os"
	"strings"
)

// Clean trims whitespace and then strips one or more layers of a single
// matching pair of leading/trailing quote characters (' or ").
func Clean(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 {
		first := s[0]
		last := s[len(s)-1]
		if (first == '"' || first == '\'') && first == last {
			s = strings.TrimSpace(s[1 : len(s)-1])
			continue
		}
		break
	}
	return s
}

// Get returns the cleaned value of the given environment variable, or def
// if it is unset or empty after cleaning.
func Get(key, def string) string {
	v := Clean(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}
