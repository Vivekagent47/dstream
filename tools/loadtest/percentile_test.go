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

// Nearest-rank edge cases beyond the 1..100 table above.
func TestPercentileEdges(t *testing.T) {
	cases := []struct {
		name   string
		sorted []float64
		q      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"empty q0", []float64{}, 0, 0},
		{"one element p50", []float64{7}, 0.5, 7},
		{"one element p99", []float64{7}, 0.99, 7},
		{"one element q0", []float64{7}, 0, 7},
		{"one element q1", []float64{7}, 1, 7},
		{"four, exact rank p50 -> 2nd", []float64{1, 2, 3, 4}, 0.5, 2},
		{"four, ceil rank p51 -> 3rd", []float64{1, 2, 3, 4}, 0.51, 3},
		{"four, p25 exact -> 1st", []float64{1, 2, 3, 4}, 0.25, 1},
		{"four, p75 exact -> 3rd", []float64{1, 2, 3, 4}, 0.75, 3},
		{"four, p99 -> last", []float64{1, 2, 3, 4}, 0.99, 4},
		{"q tiny -> first (rank clamps to 1)", []float64{5, 6, 7}, 1e-12, 5},
		{"q negative -> first", []float64{5, 6, 7}, -3, 5},
		{"q above 1 -> last", []float64{5, 6, 7}, 2.5, 7},
		{"float fuzz 0.3*10 stays rank 3", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.3, 3},
	}
	for _, c := range cases {
		if got := percentile(c.sorted, c.q); got != c.want {
			t.Errorf("%s: percentile(%v, %v) = %v, want %v", c.name, c.sorted, c.q, got, c.want)
		}
	}
}
