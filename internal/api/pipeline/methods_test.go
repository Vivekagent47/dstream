package pipeline

import "testing"

func TestValidateMethods(t *testing.T) {
	if _, err := validateMethods(nil); err == nil {
		t.Fatal("empty set should error")
	}
	if _, err := validateMethods([]string{"GET"}); err == nil {
		t.Fatal("GET should error")
	}
	if _, err := validateMethods([]string{"TRACE"}); err == nil {
		t.Fatal("unknown method should error")
	}
	got, err := validateMethods([]string{"post", "Put"})
	if err != nil {
		t.Fatalf("valid set errored: %v", err)
	}
	if len(got) != 2 || got[0] != "POST" || got[1] != "PUT" {
		t.Fatalf("want normalized [POST PUT], got %v", got)
	}
}
