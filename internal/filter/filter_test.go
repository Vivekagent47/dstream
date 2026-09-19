package filter

import "testing"

func TestMatch(t *testing.T) {
	pay := []byte(`{"type":"invoice.paid","amount":150}`)
	cases := []struct {
		name     string
		expr     string
		outbound bool
		want     bool
		wantErr  bool
	}{
		{"match", `payload.type == "invoice.paid" && payload.amount > 100`, false, true, false},
		{"no match", `payload.amount > 1000`, false, false, false},
		{"header", `headers["x-src"] == "stripe"`, false, true, false},
		{"has guard true", `has(payload.type)`, false, true, false},
		{"has guard false", `has(payload.missing)`, false, false, false},
		{"missing field errs (fail-open handled by caller)", `payload.missing == 1`, false, false, true},
		{"outbound vars", `event_type == "user.created" && size(channels) > 0`, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := Meta{}
			hdr := map[string]string{"x-src": "stripe"}
			if c.outbound {
				meta = Meta{EventType: "user.created", Channels: []string{"a"}}
			}
			got, err := Match(c.expr, c.outbound, pay, hdr, meta)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestCostBound(t *testing.T) {
	// audit I2: a valid-bool expr whose nested constant-list comprehensions would
	// evaluate an enormous iteration count must be stopped by the runtime cost
	// bound (→ error, caller fails open) rather than pegging a worker goroutine.
	// Payload is tiny so the comprehension is the only cost source.
	L := `[0,1,2,3,4,5,6,7,8,9]`
	expr := L + `.all(a,` + L + `.all(b,` + L + `.all(c,` + L + `.all(d,` + L + `.all(e, a+b+c+d+e >= 0)))))`
	if _, err := Match(expr, false, []byte(`{}`), nil, Meta{}); err == nil {
		t.Fatal("want cost-limit error on pathological comprehension")
	}
	// A normal predicate stays well under budget and evaluates cleanly.
	got, err := Match(`payload.amount > 100`, false, []byte(`{"amount":150}`), nil, Meta{})
	if err != nil {
		t.Fatalf("normal expr should be well under budget: %v", err)
	}
	if !got {
		t.Fatal("normal expr: want true")
	}
}

func TestOutboundEmptyMeta(t *testing.T) {
	// audit item 4: an outbound expr referencing event_type/channels must not
	// error when the delivery has empty event_type AND nil channels (else a
	// spurious fail-open). The vars are bound unconditionally for outbound.
	got, err := Match(`event_type == "" && size(channels) == 0`, true, []byte(`{}`), nil, Meta{})
	if err != nil {
		t.Fatalf("outbound expr errored on empty meta: %v", err)
	}
	if !got {
		t.Fatal("want true for empty event_type + empty channels")
	}
}

func TestCompileError(t *testing.T) {
	if _, err := Compile(`payload.type ==`, false); err == nil {
		t.Fatal("want compile error on malformed expr")
	}
	// non-bool result type is a compile error
	if _, err := Compile(`payload.amount`, false); err == nil {
		t.Fatal("want compile error: expr must be boolean")
	}
}
