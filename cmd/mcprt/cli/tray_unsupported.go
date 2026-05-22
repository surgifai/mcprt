//go:build !darwin && !windows

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func trayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tray",
		Short: "System tray app (not available on this platform)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("mcprt tray requires a desktop environment (macOS or Windows)\n" +
				"On Linux, use the web dashboard instead: http://localhost:9090/_dashboard/")
		},
	}
}
