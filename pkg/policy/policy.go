// Package policy validates manifest configs against mcprt's security posture.
// Key stance: Streamable HTTP only — STDIO transport is refused unconditionally.
package policy

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/surgifai-com/mcprt/pkg/manifest"
)

// Severity of a policy violation.
type Severity string

const (
	SeverityError   Severity = "error"   // hard refusal — server will not load
	SeverityWarning Severity = "warning" // informational — server loads with a log entry
)

// Violation describes a single policy breach.
type Violation struct {
	Server   string // "" for runtime-level violations
	Rule     string // machine-readable rule ID
	Severity Severity
	Message  string
}

func (v Violation) String() string {
	if v.Server != "" {
		return fmt.Sprintf("[%s] server %q: %s", v.Severity, v.Server, v.Message)
	}
	return fmt.Sprintf("[%s] runtime: %s", v.Severity, v.Message)
}

// Validate runs all policy checks on cfg and returns any violations.
// Errors are hard blocks; warnings are surfaced but do not prevent load.
func Validate(cfg *manifest.Config) []Violation {
	var vv []Violation
	vv = append(vv, validateRuntime(cfg)...)
	for name, spec := range cfg.Server {
		vv = append(vv, validateServer(name, spec)...)
	}
	return vv
}

// HasErrors returns true if any violation has severity Error.
func HasErrors(vv []Violation) bool {
	for _, v := range vv {
		if v.Severity == SeverityError {
			return true
		}
	}
	return false
}

func validateRuntime(cfg *manifest.Config) []Violation {
	var vv []Violation
	if cfg.Runtime.Listen == "" {
		vv = append(vv, Violation{
			Rule: "runtime.listen.empty", Severity: SeverityError,
			Message: "runtime.listen must be set",
		})
	}
	return vv
}

func validateServer(name string, spec manifest.ServerSpec) []Violation {
	var vv []Violation

	if spec.UpstreamURL != "" {
		return vv
	}

	if len(spec.Exec) == 0 {
		return append(vv, emptyExecViolation(name))
	}

	binary := filepath.Base(spec.Exec[0])
	allArgs := append(spec.Exec[1:], spec.Args...)
	vv = append(vv, validateStdioLauncher(name, binary, spec.AcknowledgedStdioSafe)...)
	vv = append(vv, validateStdioPackages(name, allArgs, spec.AcknowledgedStdioSafe)...)

	if !hasPortBinding(spec) {
		vv = append(vv, noPortViolation(name))
	}
	vv = append(vv, validateHostRestriction(name, allArgs, spec.AllowExternal)...)
	return vv
}

func emptyExecViolation(name string) Violation {
	return Violation{
		Server: name, Rule: "server.exec.empty", Severity: SeverityError,
		Message: "exec must have at least one element",
	}
}

func validateStdioLauncher(name, binary string, acknowledged bool) []Violation {
	for _, launcher := range []string{"node", "npx", "python", "python3", "deno", "bun"} {
		if binary != launcher {
			continue
		}
		if acknowledged {
			return []Violation{{
				Server: name, Rule: "server.stdio.launcher.acknowledged", Severity: SeverityWarning,
				Message: fmt.Sprintf("exec uses %q with acknowledged_stdio_safe=true — ensure it binds an HTTP port", binary),
			}}
		}
		return []Violation{{
			Server: name, Rule: "server.stdio.launcher", Severity: SeverityError,
			Message: fmt.Sprintf(
				"exec uses %q — a process launcher that commonly wraps STDIO MCPs. "+
					"If this binary genuinely starts an HTTP MCP server, set acknowledged_stdio_safe=true.",
				binary,
			),
		}}
	}
	return nil
}

func validateStdioPackages(name string, args []string, acknowledged bool) []Violation {
	if acknowledged {
		return nil
	}
	var vv []Violation
	for _, arg := range args {
		if frag, ok := matchingStdioPackageFragment(arg); ok {
			vv = append(vv, Violation{
				Server: name, Rule: "server.stdio.package", Severity: SeverityError,
				Message: fmt.Sprintf(
					"arg %q matches known STDIO MCP package pattern %q. "+
						"If this package exposes an HTTP interface, set acknowledged_stdio_safe=true.",
					arg, frag,
				),
			})
		}
	}
	return vv
}

func matchingStdioPackageFragment(arg string) (string, bool) {
	for _, frag := range []string{"@modelcontextprotocol/server-", "mcp-server-", "-mcp-server"} {
		if strings.Contains(arg, frag) {
			return frag, true
		}
	}
	return "", false
}

func noPortViolation(name string) Violation {
	return Violation{
		Server: name, Rule: "server.no_port", Severity: SeverityError,
		Message: "server args do not contain ${MCPRT_PORT} or a --port/--bind flag; " +
			"mcprt cannot determine where to proxy requests",
	}
}

func validateHostRestriction(name string, args []string, allowExternal bool) []Violation {
	if allowExternal {
		return nil
	}
	var vv []Violation
	for _, arg := range args {
		if arg == "0.0.0.0" || arg == "::0" {
			vv = append(vv, Violation{
				Server: name, Rule: "server.external_bind", Severity: SeverityError,
				Message: fmt.Sprintf(
					"arg %q binds all interfaces. Set allow_external=true if this is intentional.", arg,
				),
			})
		}
	}
	return vv
}

// hasPortBinding returns true if the server spec pins a port or contains a
// port substitution token or an explicit --port / -p flag in its arguments or env.
func hasPortBinding(spec manifest.ServerSpec) bool {
	if spec.Port > 0 {
		return true
	}
	if hasMCPRTPortEnv(spec.Env) {
		return true
	}
	allArgs := append(spec.Exec[1:], spec.Args...)
	for i := range allArgs {
		if argumentHasPortBinding(allArgs, i) {
			return true
		}
	}
	return false
}

func hasMCPRTPortEnv(env map[string]string) bool {
	for _, v := range env {
		if strings.Contains(v, "${MCPRT_PORT}") {
			return true
		}
	}
	return false
}

func argumentHasPortBinding(args []string, i int) bool {
	arg := args[i]
	if strings.Contains(arg, "${MCPRT_PORT}") {
		return true
	}
	if arg == "--port" || arg == "-p" || arg == "--bind" {
		return i+1 < len(args)
	}
	return strings.HasPrefix(arg, "--port=") || strings.HasPrefix(arg, "--bind=")
}
