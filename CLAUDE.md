# awsocks

SOCKS5 proxy via AWS SSM for secure EC2 access.

## Quick Reference

```bash
make                 # Build awsocks binary
make test-unit       # Run unit tests
make build-agent     # Build Linux agent for VM
```

## Architecture

```
cmd/awsocks/     # CLI entrypoint
cmd/agent/       # VM agent (Linux, runs as init)
internal/
  awsapi/        # AWS API client (EC2/SSM via VM NAT)
  backend/       # Backend interface + SSM implementation
  clock/         # Time abstraction for testing
  config/        # TOML config, CLI flags, merge logic
  credentials/   # AWS credential provider with auto-refresh
  ec2/           # Instance resolver (Name tag search)
  log/           # Structured logging
  mux/           # AgentMux: shared vsock multiplexer (host <-> VM agent)
  protocol/      # vsock message protocol (host <-> VM)
  proxy/         # SOCKS5 server + Manager (VM lifecycle, idle timeout)
  routing/       # Domain-based routing with fallback support
  testutil/      # Test helpers and fake SSM server
  ui/            # Interactive profile selection
  vm/            # macOS Virtualization.framework wrapper
```

## Key Patterns

- **Backend interface**: `internal/backend/backend.go` - all backends implement `Dial()`, `OnCredentialUpdate()`
- **SSM state machine**: StateIdle → StateInitializing → StateActive → StateTransitioning
- **AgentMux**: `internal/mux/agent_mux.go` - single multiplexer over vsock, prevents dual-reader bugs
- **Testing**: Use interfaces + mocks (testify/mock), table-driven tests
- **Build tags**: `//go:build darwin` for VM code, `//go:build linux` for agent

## Development (TDD)

Follow Test-Driven Development:
1. **Red**: Write a failing test first
2. **Green**: Write minimal code to pass the test
3. **Refactor**: Clean up while keeping tests green

Run tests frequently: `make test-unit`

## AWS API Route

| Route | Flag | Description |
|-------|------|-------------|
| direct | default | Host calls AWS APIs directly |
| vm | `--aws-api-route vm` | AWS APIs routed through VM NAT |

## Routing

Routes determine how connections are handled:
- `proxy` - Route through EC2 via SSM (default)
- `direct` - Direct from host (bypass proxy)
- `vm-direct` - Direct from VM NAT (VM mode only)
- `block` - Block connection

Fallback: When `proxy` fails with "No route to host", automatically retries via `direct` or `vm-direct`.

## Lazy Connection Mode

`--lazy` flag (default: true) defers AWS initialization until first proxy request:
- Faster startup (no AWS API calls)
- During initialization, requests use direct/vm-direct to avoid blocking
- Useful when OIDC auth requires browser access through the proxy

## Idle Timeout

`--idle-timeout` suspends the EC2 instance after a period of inactivity.
On next proxy request, the instance is automatically restarted (handles stopping → stopped → running transitions).

## AWS SDK Strategy

To minimize binary size, avoid AWS SDK service packages:
- `internal/ec2/http_client.go` - Direct HTTP with SigV4 (Query/XML)
- `internal/backend/ssm/http_client.go` - Direct HTTP with SigV4 (JSON 1.1)
- Keep `aws-sdk-go-v2/config` for SSO/profile support (too complex to replace)

## Commit Style

- Message in English, semantic commit format
