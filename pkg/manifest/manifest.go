// Package manifest loads and parses mcprt.toml configuration files.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level mcprt.toml structure.
type Config struct {
	Runtime RuntimeConfig        `toml:"runtime"`
	Server  map[string]ServerSpec `toml:"server"`
}

// RuntimeConfig holds daemon-wide settings.
type RuntimeConfig struct {
	Listen      string `toml:"listen"`
	LogLevel    string `toml:"log_level"`
	GracePeriod string `toml:"grace_period"`
	// APIKey protects the /_api/ REST endpoints. Supports ${VAR} substitution
	// from the secrets store. Leave empty to disable auth (localhost-only deployments).
	APIKey string `toml:"api_key"`
	// SecretsFile is the path to the chmod-600 KEY=VALUE secrets file.
	// Defaults to ~/.config/mcprt/secrets.env when unset.
	SecretsFile string `toml:"secrets_file"`
}

// ServerSpec defines a single managed MCP server.
type ServerSpec struct {
	Exec              []string          `toml:"exec"`
	Args              []string          `toml:"args"`
	Env               map[string]string `toml:"env"`
	HealthPath        string            `toml:"health_path"`
	WorkingDir        string            `toml:"working_dir"`
	AllowExternal     bool              `toml:"allow_external"`
	AcknowledgedStdioSafe bool          `toml:"acknowledged_stdio_safe"`
	// Port pins the upstream port instead of using dynamic allocation.
	// Use this for servers that read their port from a config file rather
	// than a CLI argument. When set, ${MCPRT_PORT} substitution still works
	// but is not required by the policy validator.
	Port int `toml:"port"`
	// HealthTimeout overrides the global health-check deadline for this server.
	// Useful for servers with slow startup (e.g. loading large ML models).
	// Accepts Go duration strings: "30s", "1m", "500ms". Default: 5s.
	HealthTimeout string `toml:"health_timeout"`
	// HealthType controls the probe mechanism. Options:
	//   "http" (default): GET health_path, pass if status < 500
	//   "tcp":  dial the port, pass when connection is accepted (no HTTP)
	//   "none": skip health check entirely, use a 500ms fixed delay
	HealthType string `toml:"health_type"`

	// UpstreamURL, when set, configures a passthrough proxy to a remote
	// HTTP/HTTPS endpoint instead of spawning a local process. Exec, Port,
	// and health-check fields are ignored. The path suffix after the server
	// name is forwarded verbatim to the upstream.
	//
	// Example:
	//   upstream_url = "https://recon.surgifai.co"
	//   headers = { "CF-Access-Client-Id" = "...", "CF-Access-Client-Secret" = "..." }
	UpstreamURL string            `toml:"upstream_url"`
	// Headers are injected into every proxied request (e.g. CF Access service token).
	Headers     map[string]string `toml:"headers"`
}

// DefaultRuntimeConfig returns conservative defaults.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		Listen:      "127.0.0.1:9090",
		LogLevel:    "info",
		GracePeriod: "5s",
	}
}

// Load parses a mcprt.toml file and returns the config.
func Load(path string) (*Config, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %q: %w", path, err)
	}
	return parse(data)
}

// Parse parses raw TOML bytes — exported so policy.Validate can be called
// without touching the filesystem.
func Parse(data []byte) (*Config, error) {
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	cfg := &Config{
		Runtime: DefaultRuntimeConfig(),
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	// Expand ~ in each server's working dir and exec paths.
	for name, spec := range cfg.Server {
		if len(spec.Exec) > 0 {
			spec.Exec[0] = expandHome(spec.Exec[0])
		}
		spec.WorkingDir = expandHome(spec.WorkingDir)
		cfg.Server[name] = spec
	}
	cfg.Runtime.Listen = expandHome(cfg.Runtime.Listen)
	return cfg, nil
}

// varPattern matches ${VAR_NAME} placeholders (ASCII identifiers only).
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandVars substitutes ${VAR} placeholders across all string fields in cfg
// using the provided vars map. ${MCPRT_PORT} is left untouched — it is
// substituted at process-spawn time by the supervisor.
// Returns the list of placeholder names that were referenced but not found in vars.
func ExpandVars(cfg *Config, vars map[string]string) []string {
	seen := map[string]bool{}
	var missing []string

	expand := func(s string) string {
		return varPattern.ReplaceAllStringFunc(s, func(m string) string {
			key := m[2 : len(m)-1]
			if key == "MCPRT_PORT" {
				return m
			}
			if val, ok := vars[key]; ok {
				return val
			}
			if !seen[key] {
				seen[key] = true
				missing = append(missing, key)
			}
			return m
		})
	}

	cfg.Runtime.APIKey = expand(cfg.Runtime.APIKey)

	for name, spec := range cfg.Server {
		for i, e := range spec.Exec {
			spec.Exec[i] = expand(e)
		}
		for i, a := range spec.Args {
			spec.Args[i] = expand(a)
		}
		for k, v := range spec.Env {
			spec.Env[k] = expand(v)
		}
		for k, v := range spec.Headers {
			spec.Headers[k] = expand(v)
		}
		spec.UpstreamURL = expand(spec.UpstreamURL)
		cfg.Server[name] = spec
	}
	return missing
}

// FindVarRefs returns the unique set of ${VAR} placeholder names referenced
// anywhere in the config. Used by `mcprt secrets verify`.
func FindVarRefs(cfg *Config) []string {
	seen := map[string]bool{}
	var refs []string

	collect := func(s string) {
		for _, m := range varPattern.FindAllStringSubmatch(s, -1) {
			key := m[1]
			if key == "MCPRT_PORT" {
				continue
			}
			if !seen[key] {
				seen[key] = true
				refs = append(refs, key)
			}
		}
	}

	collect(cfg.Runtime.APIKey)
	for _, spec := range cfg.Server {
		for _, e := range spec.Exec {
			collect(e)
		}
		for _, a := range spec.Args {
			collect(a)
		}
		for _, v := range spec.Env {
			collect(v)
		}
		for _, v := range spec.Headers {
			collect(v)
		}
		collect(spec.UpstreamURL)
	}
	return refs
}

func expandHome(s string) string {
	if !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, s[2:])
}
