package main

import "testing"

func TestJoinForwardURL(t *testing.T) {
	cases := []struct {
		name    string
		forward string
		evPath  string
		want    string
	}{
		{"empty path returns base", "http://localhost:3000", "", "http://localhost:3000"},
		{"root base + path", "http://localhost:3000", "/webhooks/stripe", "http://localhost:3000/webhooks/stripe"},
		{"base with path joins", "http://localhost:3000/api", "/hook", "http://localhost:3000/api/hook"},
		{"trailing slash trimmed", "http://localhost:3000/api/", "/hook", "http://localhost:3000/api/hook"},
		{"query preserved from event", "http://localhost:3000", "/hook?a=1&b=2", "http://localhost:3000/hook?a=1&b=2"},
		{"empty event query keeps base", "http://localhost:3000/base", "/x", "http://localhost:3000/base/x"},
		{"unparseable base returns base", "://bad", "/x", "://bad"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := joinForwardURL(c.forward, c.evPath); got != c.want {
				t.Errorf("joinForwardURL(%q, %q) = %q, want %q", c.forward, c.evPath, got, c.want)
			}
		})
	}
}
