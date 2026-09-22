package main

import "testing"

// nearest-rank on 1..100: p50=50, p95=95, p99=99, max(q>=1)=100.
func TestPercentile(t *testing.T) {
	sorted := make([]float64, 100)
	for i := range sorted {
		sorted[i] = float64(i + 1) // 1..100
	}

	cases := []struct {
		q    float64
		want float64
	}{
		{0.50, 50},
		{0.95, 95},
		{0.99, 99},
		{1.0, 100},
		{0.0, 1},
	}
	for _, c := range cases {
		if got := percentile(sorted, c.q); got != c.want {
			t.Errorf("percentile(1..100, %v) = %v, want %v", c.q, got, c.want)
		}
	}

	if got := percentile(nil, 0.99); got != 0 {
		t.Errorf("percentile(empty, 0.99) = %v, want 0", got)
	}
}
