package cli

import (
	"os"
	"path/filepath"
)

const mcprtConfigDir = ".config"

func defaultConfigPath(filename string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, mcprtConfigDir, "mcprt", filename)
}
