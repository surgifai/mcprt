package cli

import (
	"fmt"
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
			applySecretsFileFromManifest(cmd, &secretsFile)
			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&secretsFile, "secrets-file", defaultConfigPath("secrets.env"), "path to secrets file")
	openStore := secretsStoreOpener(&secretsFile)
	cmd.AddCommand(listSecretsCmd(openStore))
	cmd.AddCommand(setSecretCmd(openStore))
	cmd.AddCommand(getSecretCmd(openStore))
	cmd.AddCommand(deleteSecretCmd(openStore))
	cmd.AddCommand(verifySecretsCmd(openStore))
	cmd.AddCommand(importSecretsCmd(openStore))

	return cmd
}

type openSecretsStore func() (*secrets.Store, error)

func secretsStoreOpener(secretsFile *string) openSecretsStore {
	return func() (*secrets.Store, error) {
		store, err := secrets.Open(*secretsFile)
		if err != nil {
			return nil, fmt.Errorf("cannot open secrets file %q: %w", *secretsFile, err)
		}
		return store, nil
	}
}

func applySecretsFileFromManifest(cmd *cobra.Command, secretsFile *string) {
	if cmd.Flags().Changed("secrets-file") {
		return
	}
	cfg, err := manifest.Load(configPath(cmd))
	if err == nil && cfg.Runtime.SecretsFile != "" {
		*secretsFile = cfg.Runtime.SecretsFile
	}
}

func configPath(cmd *cobra.Command) string {
	cfgFlag := cmd.Root().Flag("config")
	if cfgFlag != nil && cfgFlag.Value.String() != "" {
		return cfgFlag.Value.String()
	}
	return defaultConfigPath("mcprt.toml")
}

func listSecretsCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
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
	}
}

func setSecretCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
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
	}
}

func getSecretCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
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
	}
}

func deleteSecretCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
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
	}
}

func verifySecretsCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Check which ${VAR} references in mcprt.toml are present in the secrets file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := manifest.Load(configPath(cmd))
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			refs := manifest.FindVarRefs(cfg)
			if len(refs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no ${VAR} references found in config")
				return nil
			}
			vars, err := loadSecretVars(openStore)
			if err != nil {
				return err
			}
			missing := printSecretStatus(cmd, refs, vars)
			if missing > 0 {
				return fmt.Errorf("%d missing — run: mcprt secrets set KEY VALUE", missing)
			}
			return nil
		},
	}
}

func loadSecretVars(openStore openSecretsStore) (map[string]string, error) {
	store, err := openStore()
	if err != nil {
		return nil, err
	}
	vars, err := store.LoadAll()
	if err != nil {
		return nil, fmt.Errorf("reading secrets: %w", err)
	}
	return vars, nil
}

func printSecretStatus(cmd *cobra.Command, refs []string, vars map[string]string) int {
	sort.Strings(refs)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "VAR\tSTATUS")
	fmt.Fprintln(w, strings.Repeat("─", 44))
	missing := 0
	for _, k := range refs {
		if _, ok := vars[k]; ok {
			fmt.Fprintf(w, "%s\t✓ resolved\n", k)
			continue
		}
		fmt.Fprintf(w, "%s\t✗ MISSING\n", k)
		missing++
	}
	w.Flush()
	return missing
}

func importSecretsCmd(openStore openSecretsStore) *cobra.Command {
	return &cobra.Command{
		Use:   "import FILE",
		Short: "Import KEY=VALUE pairs from a .env file into the secrets file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			return importSecretValues(cmd, store, tmp)
		},
	}
}

func importSecretValues(cmd *cobra.Command, store *secrets.Store, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := store.Set(k, values[k]); err != nil {
			return fmt.Errorf("storing %s: %w", k, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", k)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "imported %d secret(s) → %s\n", len(values), store.Path())
	return nil
}
