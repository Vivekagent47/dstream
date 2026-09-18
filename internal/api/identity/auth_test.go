package identity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// VerifyMagicLink issues a session cookie and is CSRF-exempt (no session exists
// yet), so it must reject non-JSON content types up front — otherwise a
// cross-site text/plain form POST could plant the attacker's token (session
// fixation). The content-type check precedes any DB/Signer use, so a zero-value
// Handlers is enough to exercise it.
func TestVerifyMagicLinkRejectsNonJSON(t *testing.T) {
	var h Handlers
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/verify",
		strings.NewReader(`{"token":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	h.VerifyMagicLink(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain body: got %d want 415", rec.Code)
	}
}
