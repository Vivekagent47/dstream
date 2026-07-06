package ingest

import "strings"

// methodAllowed reports whether method is in the source's allowed set
// (case-insensitive; allowed values are stored uppercase).
func methodAllowed(allowed []string, method string) bool {
	m := strings.ToUpper(method)
	for _, a := range allowed {
		if a == m {
			return true
		}
	}
	return false
}
