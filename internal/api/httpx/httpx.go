// Package httpx holds the small helpers shared by every api subpackage:
// JSON response writing and nullable-column comparison/deref used when
// diffing rows for audit metadata.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON encodes v as the response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Err writes a JSON error envelope: {"error": msg}.
func Err(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// RawJSONOrEmpty turns a JSONB column ([]byte) into a json.RawMessage so the
// response encoder emits it verbatim instead of base64-ing it. An empty or
// nil slice becomes `{}` so the JSON output is always a valid object.
func RawJSONOrEmpty(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

// StrPtrEq returns true if both pointers are nil, or both point to equal
// strings. Used for diffing nullable columns in audit metadata.
func StrPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// Int32PtrEq is the *int32 sibling of StrPtrEq.
func Int32PtrEq(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// DerefString returns nil for nil; the value otherwise.
func DerefString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// DerefInt32 returns nil for nil; the value otherwise (so JSON encodes the
// missing value as null instead of zero).
func DerefInt32(i *int32) any {
	if i == nil {
		return nil
	}
	return *i
}
