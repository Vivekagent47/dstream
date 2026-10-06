package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	root := &cobra.Command{
		Use:           "dstream",
		Short:         "dstream — webhook management, monitoring, and testing platform",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errOut)

	root.AddCommand(
		serverCmd(),
		workerCmd(),
		cliCmd(),
		migrateCmd(),
		adminCmd(),
	)

	return root.Execute()
}
