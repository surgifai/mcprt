# mcprt — Local Streamable HTTP MCP process supervisor

> **Project graph entry**: /Users/victorqnguyen/surgifai/CLAUDE.md (umbrella).
> Last verified: pending — session 0307 actual fixed-endpoint owner proof outstanding.

## Graph edges

- Parent: /Users/victorqnguyen/surgifai/CLAUDE.md
- Runtime and operator contract: README.md
- Listener lifecycle and supporting tests: pkg/runtime/runtime.go; pkg/runtime/runtime_test.go
- Child lifecycle and supporting tests: pkg/supervisor/supervisor.go; pkg/supervisor/supervisor_test.go
- Authenticated status and supporting tests: pkg/api/api.go; pkg/api/api_test.go

## Landmarks

- 2026-10-09 — Session 0307 adds authenticated listener and child lifetime identifiers to existing server status; original routes and process ownership remain authoritative. Local component tests passed before commit; real builder-to-owner proof remains pending.

## Land-mines

- Status instance identifiers describe live process lifetimes; they do not attest executable content or authorize tool calls.
- Do not populate listener identity from configuration before a listener exists: reloads may change configuration without changing the active socket.
- Child identifiers must disappear when the supervised process stops or exits; retaining an old identifier could misbind an owner receipt.
- Implementation validation does not authorize restarting an operator daemon or refreshing a downstream graph.
