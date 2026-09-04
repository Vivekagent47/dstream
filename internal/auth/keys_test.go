package auth

import "testing"

func TestParseAPIKeyHandlesUnderscoreInPrefix(t *testing.T) {
	// 12-char prefix containing '_', secret containing '_' too.
	raw := "dsk_a_cdefghijkl_sec_ret_val"
	prefix, secret, ok := parseAPIKey(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if prefix != "a_cdefghijkl" {
		t.Fatalf("prefix: got %q want %q", prefix, "a_cdefghijkl")
	}
	if secret != "sec_ret_val" {
		t.Fatalf("secret: got %q want %q", secret, "sec_ret_val")
	}
}

func TestParseAPIKeyRoundTripsGeneratedKey(t *testing.T) {
	for i := 0; i < 200; i++ {
		full, prefix, _, err := NewAPIKey()
		if err != nil {
			t.Fatalf("NewAPIKey: %v", err)
		}
		gotPrefix, gotSecret, ok := parseAPIKey(full)
		if !ok || gotPrefix != prefix || gotSecret == "" {
			t.Fatalf("round-trip failed: full=%q prefix=%q ok=%v got=%q", full, prefix, ok, gotPrefix)
		}
	}
}

func TestParseAPIKeyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "nope", "dsk_", "dsk_short_", "dsk_" + "abcdefghijkl"} {
		if _, _, ok := parseAPIKey(bad); ok {
			t.Fatalf("expected reject for %q", bad)
		}
	}
}
