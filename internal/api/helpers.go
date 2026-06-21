package api

import (
	"bytes"
	"encoding/json"
)

// rawJSONOrEmpty turns a JSONB column ([]byte) into a json.RawMessage so the
// response encoder emits it verbatim instead of base64-ing it. An empty or
// nil slice becomes `{}` so the JSON output is always a valid object.
func rawJSONOrEmpty(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

// strPtrEq returns true if both pointers are nil, or both point to equal
// strings. Used for diffing nullable columns in audit metadata.
func strPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// int32PtrEq is the *int32 sibling of strPtrEq.
func int32PtrEq(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// derefString returns "" for nil; the value otherwise.
func derefString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// derefInt32 returns nil for nil; the value otherwise (so JSON encodes the
// missing value as null instead of zero).
func derefInt32(i *int32) any {
	if i == nil {
		return nil
	}
	return *i
}

// bytesEq compares two []byte slices for equality.
func bytesEq(a, b []byte) bool { return bytes.Equal(a, b) }
