package pipeline

import "testing"

func i32(v int32) *int32 { return &v }

func TestValidateRetryParams(t *testing.T) {
	cases := []struct {
		name string
		req  patchConnectionReq
		want string // "" means valid
	}{
		{"empty patch", patchConnectionReq{}, ""},
		{"max_retries ok", patchConnectionReq{MaxRetries: i32(100)}, ""},
		{"max_retries neg", patchConnectionReq{MaxRetries: i32(-1)}, "max_retries must be 0..100"},
		{"max_retries too big", patchConnectionReq{MaxRetries: i32(101)}, "max_retries must be 0..100"},
		{"jitter ok", patchConnectionReq{RetryJitterPct: i32(0)}, ""},
		{"jitter too big", patchConnectionReq{RetryJitterPct: i32(101)}, "retry_jitter_pct must be 0..100"},
		{"jitter neg", patchConnectionReq{RetryJitterPct: i32(-1)}, "retry_jitter_pct must be 0..100"},
		{"base zero", patchConnectionReq{RetryBaseMs: i32(0)}, "retry_base_ms must be 1..604800000"},
		{"base too big", patchConnectionReq{RetryBaseMs: i32(604800001)}, "retry_base_ms must be 1..604800000"},
		{"base max ok", patchConnectionReq{RetryBaseMs: i32(604800000)}, ""},
		{"cap zero", patchConnectionReq{RetryCapMs: i32(0)}, "retry_cap_ms must be 1..604800000"},
		{"cap too big", patchConnectionReq{RetryCapMs: i32(604800001)}, "retry_cap_ms must be 1..604800000"},
		{"base>cap", patchConnectionReq{RetryBaseMs: i32(10), RetryCapMs: i32(5)}, "retry_base_ms must be <= retry_cap_ms"},
		{"base==cap ok", patchConnectionReq{RetryBaseMs: i32(5), RetryCapMs: i32(5)}, ""},
		{"base<cap ok", patchConnectionReq{RetryBaseMs: i32(5), RetryCapMs: i32(10)}, ""},
		// Only one of base/cap present: not cross-checked (ponytail comment on fn).
		{"base alone high ok vs nothing", patchConnectionReq{RetryBaseMs: i32(1000)}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateRetryParams(c.req); got != c.want {
				t.Errorf("validateRetryParams = %q, want %q", got, c.want)
			}
		})
	}
}
