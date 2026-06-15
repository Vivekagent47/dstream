package main

import (
	"github.com/spf13/cobra"
)

func adminCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin",
		Short: "Admin operations (promote super-admin, etc.)",
	}
	c.AddCommand(promoteCmd())
	return c
}

func promoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "promote <email>",
		Short: "Promote a user to super-admin",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// TODO(phase-1.3): connect to DB, UPDATE users SET is_super_admin = true WHERE email = $1.
			_ = args[0]
			return nil
		},
	}
}
