package main

import (
	"github.com/spf13/cobra"
)

func cliCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cli",
		Short: "Local development CLI (tunnel, replay, listen)",
	}
	c.AddCommand(listenCmd())
	return c
}

func listenCmd() *cobra.Command {
	var source, forward string
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Forward events from a source to a local URL via WebSocket tunnel",
		RunE: func(_ *cobra.Command, _ []string) error {
			// TODO(phase-1.3): implement WebSocket tunnel to /api/cli/connect.
			_ = source
			_ = forward
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Source ID or name to listen on (required)")
	cmd.Flags().StringVar(&forward, "forward", "http://localhost:3000", "Local URL to forward events to")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}
