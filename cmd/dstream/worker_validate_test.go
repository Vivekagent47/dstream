package main

import (
	"testing"
	"time"

	"github.com/Vivekagent47/dstream/internal/config"
)

// TestValidateWorkerRuntime guards the worker boot checks: reject config that
// silently breaks liveness (concurrency 0, stall timeout at/below the delivery
// timeout) or leaks tokens (dev mode on a non-localhost URL).
func TestValidateWorkerRuntime(t *testing.T) {
	ok := func() config.Config {
		return config.Config{
			DevMode:       false,
			PublicBaseURL: "https://dstream.example.com",
			Worker:        config.WorkerConfig{Concurrency: 2, StallTimeout: 60 * time.Second},
		}
	}

	cases := []struct {
		name    string
		mutate  func(c *config.Config)
		wantErr bool
	}{
		{"valid", func(*config.Config) {}, false},
		{"concurrency zero", func(c *config.Config) { c.Worker.Concurrency = 0 }, true},
		{"concurrency negative", func(c *config.Config) { c.Worker.Concurrency = -1 }, true},
		{"stall zero", func(c *config.Config) { c.Worker.StallTimeout = 0 }, true},
		{"stall equals delivery timeout", func(c *config.Config) { c.Worker.StallTimeout = 30 * time.Second }, true},
		{"stall below delivery timeout", func(c *config.Config) { c.Worker.StallTimeout = 10 * time.Second }, true},
		{"devmode on public url", func(c *config.Config) { c.DevMode = true }, true},
		{"devmode on localhost ok", func(c *config.Config) {
			c.DevMode = true
			c.PublicBaseURL = "http://localhost:3000"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ok()
			tc.mutate(&c)
			err := validateWorkerRuntime(c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateWorkerRuntime err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
