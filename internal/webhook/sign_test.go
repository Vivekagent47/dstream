package webhook

import (
	"strings"
	"testing"
)

// Canonical Standard Webhooks / Svix example — reproducing it proves our
// signatures verify with any Standard Webhooks client library.
func TestSignPinnedVector(t *testing.T) {
	got, err := Sign(
		"whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
		"msg_p5jXN8AQM9LWM0D4loKWxJek",
		1614265330,
		[]byte(`{"test": 2432232314}`),
	)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	if got != want {
		t.Fatalf("signature mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestGenerateSecretRoundTrip(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if !strings.HasPrefix(s, "whsec_") {
		t.Fatalf("missing prefix: %q", s)
	}
	if _, err := Sign(s, "msg_x", 1, []byte("{}")); err != nil {
		t.Fatalf("generated secret unusable: %v", err)
	}
}
