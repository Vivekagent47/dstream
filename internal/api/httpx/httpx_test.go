package httpx

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 201, map[string]int{"n": 7})
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["n"] != 7 {
		t.Errorf("body = %v, want n:7", got)
	}
}

func TestErr(t *testing.T) {
	rec := httptest.NewRecorder()
	Err(rec, 400, "bad")
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != "bad" {
		t.Errorf("body = %v, want error:bad", got)
	}
}

func TestRawJSONOrEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"nil", nil, "{}"},
		{"empty", []byte{}, "{}"},
		{"object", []byte(`{"a":1}`), `{"a":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(RawJSONOrEmpty(c.in)); got != c.want {
				t.Errorf("RawJSONOrEmpty(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// Verify it rides through the encoder verbatim (not base64'd).
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, map[string]any{"meta": RawJSONOrEmpty([]byte(`{"k":"v"}`))})
	if got := rec.Body.String(); got != `{"meta":{"k":"v"}}`+"\n" {
		t.Errorf("encoded = %q", got)
	}
}

func TestStrPtrEq(t *testing.T) {
	s, s2, other := "x", "x", "y"
	cases := []struct {
		name string
		a, b *string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"a nil", nil, &s, false},
		{"b nil", &s, nil, false},
		{"equal", &s, &s2, true},
		{"differ", &s, &other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StrPtrEq(c.a, c.b); got != c.want {
				t.Errorf("StrPtrEq = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInt32PtrEq(t *testing.T) {
	a, a2, other := int32(1), int32(1), int32(2)
	cases := []struct {
		name string
		a, b *int32
		want bool
	}{
		{"both nil", nil, nil, true},
		{"a nil", nil, &a, false},
		{"b nil", &a, nil, false},
		{"equal", &a, &a2, true},
		{"differ", &a, &other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Int32PtrEq(c.a, c.b); got != c.want {
				t.Errorf("Int32PtrEq = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDerefString(t *testing.T) {
	if got := DerefString(nil); got != nil {
		t.Errorf("DerefString(nil) = %v, want nil", got)
	}
	s := "hi"
	if got := DerefString(&s); got != "hi" {
		t.Errorf("DerefString = %v, want hi", got)
	}
}

func TestDerefInt32(t *testing.T) {
	if got := DerefInt32(nil); got != nil {
		t.Errorf("DerefInt32(nil) = %v, want nil", got)
	}
	n := int32(5)
	if got := DerefInt32(&n); got != int32(5) {
		t.Errorf("DerefInt32 = %v, want 5", got)
	}
}
