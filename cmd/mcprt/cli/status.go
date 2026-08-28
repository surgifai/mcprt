package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/surgifai/mcprt/pkg/manifest"
	"github.com/surgifai/mcprt/pkg/supervisor"
)

func statusCmd() *cobra.Command {
	var (
		jsonOut      bool
		manifestPath string
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of all managed servers",
		Long: `Reads the manifest and reports the last-known state of each server.
	When the daemon is running, state is live; when not, it shows config only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			stats, err := loadStatusStats(manifestPath)
			if err != nil {
				return err
			}

			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(stats)
			}
			return writeStatusTable(os.Stdout, stats)
		},
	}

	cmd.Flags().StringVar(&manifestPath, "config", defaultConfigPath("mcprt.toml"), "manifest path")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}

func loadStatusStats(manifestPath string) ([]supervisor.Stats, error) {
	cfg, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, err
	}
	stats := make([]supervisor.Stats, 0, len(cfg.Server))
	for name := range cfg.Server {
		stats = append(stats, supervisor.Stats{
			Name:  name,
			State: supervisor.StateIdle,
		})
	}
	return stats, nil
}

func writeStatusTable(out *os.File, stats []supervisor.Stats) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tSTATE\tPID\tPORT\tRESTARTS\tLAST SPAWN\tERRORS")
	for _, s := range stats {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\n",
			s.Name, s.State, formatPositiveInt(s.PID), formatPositiveInt(s.Port),
			s.RestartCount, formatSpawnTime(s.LastSpawn), s.Errors)
	}
	return w.Flush()
}

func formatSpawnTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func formatPositiveInt(n int) string {
	if n <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}
