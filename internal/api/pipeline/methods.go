package pipeline

import (
	"fmt"
	"strings"
)

// supportedIngestMethods is the exact set a source may accept. GET is never
// included — dstream does not ingest webhooks over GET.
var supportedIngestMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// validateMethods uppercases each method and rejects the empty set or any
// method outside supportedIngestMethods. Returns the normalized slice.
func validateMethods(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one method required")
	}
	out := make([]string, 0, len(in))
	for _, m := range in {
		u := strings.ToUpper(strings.TrimSpace(m))
		if !supportedIngestMethods[u] {
			return nil, fmt.Errorf("unsupported method: %q", m)
		}
		out = append(out, u)
	}
	return out, nil
}
