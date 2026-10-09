// Package filter compiles and evaluates user-authored CEL predicates that
// decide, per delivery, whether an event should be sent. CEL is
// non-Turing-complete and safe to run on untrusted, multi-tenant input.
package filter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
)

// Meta carries outbound-only context (zero value for inbound).
type Meta struct {
	EventType string
	Channels  []string
}

// Program is a compiled, concurrency-safe CEL predicate.
type Program struct{ prg cel.Program }

// celCostBudget bounds actual evaluation cost per Eval so a valid-bool expression
// with pathological nested comprehensions can't peg a worker goroutine (audit
// I2). Far above any legitimate predicate over a webhook payload (field
// comparisons cost single digits), far below a comprehension blow-up. Exceeding
// it errors → the caller's fail-open policy delivers the event; work stays
// bounded, nothing is dropped.
const celCostBudget = 1_000_000

func env(outbound bool) (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.Variable("payload", cel.DynType),
		cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)),
	}
	if outbound {
		opts = append(opts,
			cel.Variable("event_type", cel.StringType),
			cel.Variable("channels", cel.ListType(cel.StringType)),
		)
	}
	return cel.NewEnv(opts...)
}

// Compile parses+checks the expression and requires a boolean result type.
// Errors here surface as HTTP 400 at write time.
func Compile(expr string, outbound bool) (*Program, error) {
	e, err := env(outbound)
	if err != nil {
		return nil, err
	}
	ast, iss := e.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("filter expression must evaluate to bool, got %s", ast.OutputType())
	}
	prg, err := e.Program(ast, cel.CostLimit(celCostBudget))
	if err != nil {
		return nil, err
	}
	return &Program{prg: prg}, nil
}

// Eval binds variables and evaluates. A non-bool or runtime error returns
// (false, err); the caller applies the fail-open policy.
func (p *Program) Eval(payload []byte, headers map[string]string, meta Meta) (bool, error) {
	// Decode with UseNumber + normalize so integers keep their exact value: a plain
	// json.Unmarshal into `any` makes every number a float64, which rounds IDs past
	// 2^53 (snowflakes, order numbers) and makes CEL comparisons against the exact
	// int literal wrong. normalizeJSON turns each json.Number into the int64/float64
	// CEL actually compares.
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var pv any
	if err := dec.Decode(&pv); err != nil {
		return false, fmt.Errorf("payload not JSON: %w", err)
	}
	pv = normalizeJSON(pv)
	// Bind event_type/channels unconditionally (audit item 4): binding them only
	// when non-empty made an outbound expr referencing them error — and fail open
	// spuriously — on a delivery with empty event_type AND nil channels. channels
	// defaults to an empty slice (not nil) so size()/comprehensions work. Inbound
	// exprs never reference these and the inbound env doesn't declare them, so the
	// extra activation keys are ignored there.
	chans := meta.Channels
	if chans == nil {
		chans = []string{}
	}
	act := map[string]any{
		"payload":    pv,
		"headers":    headers,
		"event_type": meta.EventType,
		"channels":   chans,
	}
	out, _, err := p.prg.Eval(act)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("filter did not return bool: %v", out.Value())
	}
	return b, nil
}

// normalizeJSON converts json.Number nodes (produced by a UseNumber decode) into
// the int64/float64 CEL expects, recursing through objects and arrays. An integer
// that overflows int64 falls back to float64 (the rare >9.2e18 case).
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeJSON(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeJSON(val)
		}
		return t
	default:
		return v
	}
}

// --- compiled-program cache ---

type cacheKey struct {
	hash     [32]byte
	outbound bool
}

var (
	mu    sync.Mutex
	cache = map[cacheKey]*Program{}
)

// Match is the cached delivery-time entry point.
func Match(expr string, outbound bool, payload []byte, headers map[string]string, meta Meta) (bool, error) {
	key := cacheKey{hash: sha256.Sum256([]byte(expr)), outbound: outbound}
	mu.Lock()
	p := cache[key]
	mu.Unlock()
	if p == nil {
		var err error
		p, err = Compile(expr, outbound)
		if err != nil {
			return false, err
		}
		mu.Lock()
		// ponytail: unbounded map; expr set is small (one per edge). Swap for an
		// LRU if edge count ever explodes.
		cache[key] = p
		mu.Unlock()
	}
	return p.Eval(payload, headers, meta)
}
