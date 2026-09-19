// Package transform runs user-authored JS in a locked-down goja sandbox to reshape
// a payload before delivery. The sandbox is defined by what is NOT injected:
// no require, console, fetch, timers, or filesystem — goja exposes none by
// default. A wall-clock interrupt and an output-size cap bound resource use;
// goja has no hard memory cap, so those two limits + the upstream body-size
// limit are the mitigation.
package transform

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// Compile parses the JS once (reusable across VMs). Errors → HTTP 400 on write.
func Compile(js string) (*goja.Program, error) {
	// Compiling the user source directly is fine — it defines transform() and
	// we look up the function at run time after RunProgram.
	// Non-strict: TestVMIsolation relies on `leaked = 1` creating a global (a
	// strict-mode compile rejects implicit globals with a ReferenceError). The
	// sandbox's safety comes from a fresh VM per Run + injecting nothing + the
	// timeout/output caps, not from strict mode.
	return goja.Compile("transform.js", js, false)
}

// Run executes prog in a FRESH VM, calls transform(payload, headers), and
// returns the JSON-marshalled result. Enforces timeout + output cap.
func Run(prog *goja.Program, payload []byte, headers map[string]string, timeout time.Duration, maxOut int) (out []byte, err error) {
	var pv any
	if err := json.Unmarshal(payload, &pv); err != nil {
		return nil, fmt.Errorf("payload not JSON: %w", err)
	}

	vm := goja.New()
	vm.SetMaxCallStackSize(2048)

	t := time.AfterFunc(timeout, func() { vm.Interrupt("transform timeout") })
	defer t.Stop()
	defer vm.ClearInterrupt()

	// A goja interrupt surfaces as *goja.InterruptedError via the returned
	// error; a JS throw surfaces as *goja.Exception. Recover guards any
	// host-side panic path.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("transform panic: %v", r)
		}
	}()

	if _, err = vm.RunProgram(prog); err != nil {
		return nil, fmt.Errorf("transform load: %w", err)
	}
	fn, ok := goja.AssertFunction(vm.Get("transform"))
	if !ok {
		return nil, fmt.Errorf("transform: no function named transform(payload, headers)")
	}
	res, err := fn(goja.Undefined(), vm.ToValue(pv), vm.ToValue(headers))
	if err != nil {
		return nil, fmt.Errorf("transform run: %w", err)
	}
	// spec §5: a non-object return is a terminal fail. A JS object exports to
	// map[string]interface{} and an array to []interface{}; a scalar (float64/
	// string/bool), null, or a missing return (nil) must not be marshalled into a
	// "null"/"5" body and delivered as 2xx.
	v := res.Export()
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		return nil, fmt.Errorf("transform must return an object or array, got %T", v)
	}
	out, err = json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("transform result not JSON-serializable: %w", err)
	}
	if len(out) > maxOut {
		return nil, fmt.Errorf("transform output %d bytes exceeds cap %d", len(out), maxOut)
	}
	return out, nil
}

// --- compiled-program cache ---

var (
	mu    sync.Mutex
	cache = map[[32]byte]*goja.Program{}
)

// Apply is the cached delivery-time entry point.
func Apply(js string, payload []byte, headers map[string]string, timeout time.Duration, maxOut int) ([]byte, error) {
	key := sha256.Sum256([]byte(js))
	mu.Lock()
	p := cache[key]
	mu.Unlock()
	if p == nil {
		var err error
		p, err = Compile(js)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		cache[key] = p // ponytail: unbounded; one program per edge. LRU if it grows.
		mu.Unlock()
	}
	return Run(p, payload, headers, timeout, maxOut)
}
