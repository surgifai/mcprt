package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/surgifai-com/mcprt/pkg/manifest"
	"github.com/surgifai-com/mcprt/pkg/secrets"
)

func secretsCmd() *cobra.Command {
	var secretsFile string

	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage credentials in the mcprt secrets file",
		Long: `Read and write the mcprt secrets file (chmod 600 KEY=VALUE file).

The secrets file path is resolved in this order:
  1. --secrets-file flag
  2. secrets_file in mcprt.toml
  3. Default: ~/.config/mcprt/secrets.env`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// If --secrets-file wasn't set explicitly, try to read it from the manifest.
			if !cmd.Flags().Changed("secrets-file") {
				cfgFlag := cmd.Root().Flag("config")
				cfgPath := ""
				if cfgFlag != nil {
					cfgPath = cfgFlag.Value.String()
				}
				if cfgPath == "" {
					home, _ := os.UserHomeDir()
					cfgPath = filepath.Join(home, ".config", "mcprt", "mcprt.toml")
				}
				if cfg, err := manifest.Load(cfgPath); err == nil && cfg.Runtime.SecretsFile != "" {
					secretsFile = cfg.Runtime.SecretsFile
				}
			}
			return nil
		},
	}

	home, _ := os.UserHomeDir()
	defaultPath := filepath.Join(home, ".config", "mcprt", "secrets.env")
	cmd.PersistentFlags().StringVar(&secretsFile, "secrets-file", defaultPath, "path to secrets file")

	openStore := func() (*secrets.Store, error) {
		store, err := secrets.Open(secretsFile)
		if err != nil {
			return nil, fmt.Errorf("cannot open secrets file %q: %w", secretsFile, err)
		}
		return store, nil
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all secret key names",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			keys, err := store.List()
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "(no secrets in %s)\n", store.Path())
				return nil
			}
			for _, k := range keys {
				fmt.Fprintln(cmd.OutOrStdout(), k)
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Add or update a secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			if err := store.Set(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s → %s\n", args[0], store.Path())
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "get KEY",
		Short: "Print a secret value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			val, err := store.Get(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "delete KEY",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			if err := store.Delete(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	})

	verifyCmd := &cobra.Command{
		Use:   "verify",
		Short: "Check which ${VAR} references in mcprt.toml are present in the secrets file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgFlag := cmd.Root().Flag("config")
			cfgPath := ""
			if cfgFlag != nil {
				cfgPath = cfgFlag.Value.String()
			}
			if cfgPath == "" {
				home, _ := os.UserHomeDir()
				cfgPath = filepath.Join(home, ".config", "mcprt", "mcprt.toml")
			}
			cfg, err := manifest.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			refs := manifest.FindVarRefs(cfg)
			if len(refs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no ${VAR} references found in config")
				return nil
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			vars, err := store.LoadAll()
			if err != nil {
				return fmt.Errorf("reading secrets: %w", err)
			}
			sort.Strings(refs)
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "VAR\tSTATUS")
			fmt.Fprintln(w, strings.Repeat("─", 44))
			missing := 0
			for _, k := range refs {
				if _, ok := vars[k]; ok {
					fmt.Fprintf(w, "%s\t✓ resolved\n", k)
				} else {
					fmt.Fprintf(w, "%s\t✗ MISSING\n", k)
					missing++
				}
			}
			w.Flush()
			if missing > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d missing — run: mcprt secrets set KEY VALUE\n", missing)
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.AddCommand(verifyCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "import FILE",
		Short: "Import KEY=VALUE pairs from a .env file into the secrets file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read source file directly into a temp map, then write each key.
			tmp := map[string]string{}
			if err := secrets.MergeFile(args[0], tmp); err != nil {
				return fmt.Errorf("reading %s: %w", args[0], err)
			}
			if len(tmp) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no KEY=VALUE pairs found")
				return nil
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(tmp))
			for k := range tmp {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if err := store.Set(k, tmp[k]); err != nil {
					return fmt.Errorf("storing %s: %w", k, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", k)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d secret(s) → %s\n", len(tmp), store.Path())
			return nil
		},
	})

	return cmd
}
