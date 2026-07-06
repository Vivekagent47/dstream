package ingest

import "testing"

func TestMethodAllowed(t *testing.T) {
	allowed := []string{"POST", "PUT"}
	if !methodAllowed(allowed, "POST") {
		t.Fatal("POST should be allowed")
	}
	if !methodAllowed(allowed, "put") {
		t.Fatal("put (any case) should be allowed")
	}
	if methodAllowed(allowed, "DELETE") {
		t.Fatal("DELETE not in set — should be rejected")
	}
}
