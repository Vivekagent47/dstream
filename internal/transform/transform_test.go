package transform

import (
	"testing"
	"time"
)

const to = time.Second
const maxOut = 1 << 20

func TestApply(t *testing.T) {
	pay := []byte(`{"a":1}`)
	js := `function transform(payload, headers){ return {a: payload.a, tag: headers["x"]} }`
	out, err := Apply(js, pay, map[string]string{"x": "y"}, to, maxOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1,"tag":"y"}` {
		t.Fatalf("got %s", out)
	}
}

func TestThrowIsError(t *testing.T) {
	_, err := Apply(`function transform(p,h){ throw new Error("boom") }`, []byte(`{}`), nil, to, maxOut)
	if err == nil {
		t.Fatal("want error on throw")
	}
}

func TestTimeout(t *testing.T) {
	_, err := Apply(`function transform(p,h){ while(true){} }`, []byte(`{}`), nil, 100*time.Millisecond, maxOut)
	if err == nil {
		t.Fatal("want timeout error")
	}
}

func TestNoIO(t *testing.T) {
	for _, g := range []string{"require", "fetch", "process", "setTimeout"} {
		js := `function transform(p,h){ return ` + g + ` }`
		if _, err := Apply(js, []byte(`{}`), nil, to, maxOut); err == nil {
			t.Fatalf("expected %s to be undefined/error", g)
		}
	}
}

func TestOutputCap(t *testing.T) {
	js := `function transform(p,h){ var s=""; for(var i=0;i<100;i++) s+="xxxxxxxxxx"; return {s:s} }`
	if _, err := Apply(js, []byte(`{}`), nil, to, 50); err == nil {
		t.Fatal("want output-too-large error")
	}
}

func TestNonObjectReturns(t *testing.T) {
	// spec §5: a non-object return is a terminal fail, not a silently-marshalled
	// "null"/"5"/"\"x\"" body delivered as 2xx.
	cases := map[string]string{
		"scalar number": `function transform(p,h){ return 5 }`,
		"no return":     `function transform(p,h){ }`,
		"null":          `function transform(p,h){ return null }`,
		"string":        `function transform(p,h){ return "x" }`,
		"bool":          `function transform(p,h){ return true }`,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Apply(js, []byte(`{}`), nil, to, maxOut); err == nil {
				t.Fatalf("want error for %s return", name)
			}
		})
	}
}

func TestArrayReturnOK(t *testing.T) {
	out, err := Apply(`function transform(p,h){ return [1,2,3] }`, []byte(`{}`), nil, to, maxOut)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `[1,2,3]` {
		t.Fatalf("got %s", out)
	}
}

func TestVMIsolation(t *testing.T) {
	// A global set in one run must not leak into the next (fresh VM per exec).
	js := `function transform(p,h){ if (typeof leaked !== "undefined") throw new Error("leak"); leaked = 1; return {} }`
	prog, err := Compile(js)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := Run(prog, []byte(`{}`), nil, to, maxOut); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}
