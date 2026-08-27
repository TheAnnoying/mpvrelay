// Package envcfg reads configuration from environment variables.
//
// cmd.exe's `set VAR="value"` does not strip the quotes the way a POSIX
// shell would, so a value configured that way arrives in Go's os.Getenv
// as the literal 6 characters ["value"] rather than the 5 characters
// [value]. Every value that comes from the environment is therefore run
// through Clean, which trims whitespace and then a single matching pair
// of leading/trailing quotes (repeated, in case of doubled quoting). This
// matters most on the Windows agent, but the server stub reads env vars
// too (e.g. from a docker-compose "environment:" block that a user edits
// by hand), so both sides clean every value.
package envcfg

import "strings"

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
func Get(getenv func(string) string, key, def string) string {
	v := Clean(getenv(key))
	if v == "" {
		return def
	}
	return v
}
