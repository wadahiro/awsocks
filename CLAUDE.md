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
  backend/       # Backend interface + SSM implementation
  ec2/           # Instance resolver (Name tag search)
  proxy/         # SOCKS5 server + Manager (VM/Direct modes)
  protocol/      # vsock message protocol (host <-> VM)
  credentials/   # AWS credential provider with auto-refresh
  routing/       # Domain-based routing with fallback support
  vm/            # macOS Virtualization.framework wrapper
  clock/         # Time abstraction for testing
```

## Key Patterns

- **Backend interface**: `internal/backend/backend.go` - all backends implement `Dial()`, `OnCredentialUpdate()`
- **SSM state machine**: StateIdle → StateInitializing → StateActive → StateTransitioning
- **Testing**: Use interfaces + mocks (testify/mock), table-driven tests
- **Build tags**: `//go:build darwin` for VM code, `//go:build linux` for agent

## Development (TDD)

Follow Test-Driven Development:
1. **Red**: Write a failing test first
2. **Green**: Write minimal code to pass the test
3. **Refactor**: Clean up while keeping tests green

Run tests frequently: `make test-unit`

## Modes

| Mode | Flag | Description |
|------|------|-------------|
| direct | default | Host runs Backend directly |
| vm | `--mode vm` | macOS VM via Virtualization.framework |

## Routing

Routes determine how connections are handled:
- `proxy` - Route through EC2 via SSM (default)
- `direct` - Direct from host (bypass proxy)
- `vm-direct` - Direct from VM NAT (VM mode only)
- `block` - Block connection

Fallback: When `proxy` fails with "No route to host", automatically retries via `direct` (direct mode) or `vm-direct` (VM mode).

## Lazy Connection Mode

`--lazy` flag (default: true) defers AWS initialization until first proxy request:
- Faster startup (no AWS API calls)
- During initialization, requests use direct/vm-direct to avoid blocking
- Useful when OIDC auth requires browser access through the proxy

## AWS SDK Strategy

To minimize binary size, avoid AWS SDK service packages:
- `internal/ec2/http_client.go` - Direct HTTP with SigV4 (Query/XML)
- `internal/backend/ssm/http_client.go` - Direct HTTP with SigV4 (JSON 1.1)
- Keep `aws-sdk-go-v2/config` for SSO/profile support (too complex to replace)

## Commit Style

- Message in English, semantic commit format
