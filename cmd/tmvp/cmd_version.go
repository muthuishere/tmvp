package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use: "version", Short: "Print version, protocol and registered connector types", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				fmt.Fprintf(stdout, "tmvp %s (proto %d, %s/%s)\nconnectors: %v\ntunnelers: %v\n", version, protocol.Version,
					runtime.GOOS, runtime.GOARCH, connector.Types(), connector.TunnelerTypes())
				return nil
			},
		}
	})
}
