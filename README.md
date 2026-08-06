# awsocks

SOCKS5 proxy server via AWS SSM for secure access to private VPC resources.

## What is awsocks?

awsocks creates a SOCKS5 proxy server that routes traffic through an EC2 instance via AWS Systems Manager (SSM). Unlike traditional SSH port forwarding, awsocks provides flexible domain-based routing, allowing you to selectively route traffic through the proxy or directly.

## Key Use Case: Environment Switching via VPC Private DNS

Access staging or development environments using the **same URLs as production** by leveraging VPC Private DNS resolution.

```
Browser → awsocks → SSM → EC2 → VPC Private DNS → Internal Service
        (SOCKS5)       (SSH)   (DNS resolution)
```

**How it works:**
- awsocks forwards hostnames (not IPs) to the EC2 instance
- EC2 resolves DNS using VPC's Private Hosted Zone
- Access `api.example.com` that resolves to different IPs in dev/staging/prod VPCs

**Benefits:**
- Test with production-identical URLs
- No `/etc/hosts` modifications needed
- Access private VPC resources without public IPs or SSH port opening

## Features

- **SSM-based connection** - No public IP or security group changes required
- **Domain-based routing** - Route specific domains through proxy, others directly
- **AWS SSO support** - Works with IAM Identity Center
- **Lazy connection** - Defers AWS connection until first proxy request
- **Profile management** - Multiple environment configurations
- **macOS VM mode** - Isolate proxy in lightweight VM (Virtualization.framework)

## Installation

```bash
# From source
git clone https://github.com/wadahiro/awsocks.git
cd awsocks
make

# Binary will be at ./bin/awsocks
```

## Quick Start

### 1. Create configuration

```bash
mkdir -p ~/.config/awsocks
cat > ~/.config/awsocks/config.toml << 'EOF'
[profiles.dev]
name = "bastion-dev"
aws-profile = "dev"
region = "ap-northeast-1"
ssh-key = "~/.ssh/id_ed25519"

[profiles.dev.routing]
proxy = ["*.internal.example.com"]
EOF
```

### 2. Start the proxy

```bash
awsocks start dev
```

### 3. Configure your browser

Set SOCKS5 proxy to `127.0.0.1:1080`

## Configuration

### Config file location

`~/.config/awsocks/config.toml`

### Full example

```toml
[defaults]
ssh-user = "ec2-user"
listen = "127.0.0.1:1080"

[defaults.routing]
direct = ["localhost", "127.0.0.1", "*.local"]

[profiles.dev]
name = "bastion-dev"
aws-profile = "dev"
region = "ap-northeast-1"
ssh-key = "~/.ssh/id_ed25519"
auto-start = true

[profiles.dev.routing]
proxy = ["*.internal.example.com", "*.dev.example.com"]

[profiles.staging]
name = "bastion-staging"
aws-profile = "staging"
region = "ap-northeast-1"
ssh-key = "~/.ssh/id_ed25519"

[profiles.staging.routing]
proxy = ["*.internal.example.com", "*.staging.example.com"]

[profiles.staging.routing.hosts]
"api.internal.example.com" = "10.0.1.50"
```

### Defaults

| Setting | Default | Description |
|---------|---------|-------------|
| `ssh-user` | `ec2-user` | SSH username on EC2 |
| `listen` | `127.0.0.1:1080` | SOCKS5 listen address |
| `lazy` | `true` | Defer AWS connection until first request |
| `proxy-network` | `direct` | Proxy network (`direct` or `vm`) |
| `idle-timeout` | (none) | Suspend EC2 instance after idle period (e.g., `30m`, `1h`) |

### Routing

Routes determine how traffic is handled:

| Route | Description |
|-------|-------------|
| `proxy` | Route through EC2 via SSM |
| `direct` | Direct connection from host (bypass proxy) |
| `vm-direct` | Direct from VM NAT (VM mode only) |

#### Hosts Mapping

When using SOCKS5h (remote DNS resolution), the EC2 instance may not be able to resolve certain hostnames. The `hosts` setting provides `/etc/hosts`-style hostname-to-IP mapping without modifying the EC2 instance:

```toml
[defaults.routing.hosts]
"api.prod.internal" = "10.0.1.50"
"db.prod.internal" = "10.0.1.51"

# Per-profile overrides
[profiles.dev.routing.hosts]
"api.prod.internal" = "10.0.2.50"
```

Routing decisions are made using the original hostname, then the resolved IP is used for the actual connection.

#### DNS Resolution

By default, hostnames are resolved by whichever host handles the connection: the EC2 instance for `proxy` routes, the local machine for `direct`, and the VM for `vm-direct`. When that resolver cannot see the names you need — a private hosted zone the EC2 instance is not configured for, or an internal DNS server only reachable through the tunnel — `dns` rules point specific hostnames at specific DNS servers:

```toml
[[defaults.routing.dns]]
servers = ["10.0.0.2:53"]
patterns = ["*.internal.example.com"]
```

Rules are evaluated in order and the first one matching a hostname wins. Queries use DNS over TCP.

The `via` setting selects the route that carries the DNS query itself, independent of the route used to reach the resolved address:

```toml
# Query a VPC resolver through the SSM tunnel (default)
[[defaults.routing.dns]]
via = "proxy"
servers = ["10.0.0.2:53"]
patterns = ["*.internal.example.com"]

# Query a DNS server reachable from this machine, e.g. over a VPN
[[defaults.routing.dns]]
via = "direct"
servers = ["192.168.1.1:53"]
patterns = ["*.corp.example.com"]

# Query through the VM's NAT (VM mode only)
[[defaults.routing.dns]]
via = "vm-direct"
servers = ["10.0.0.2:53"]
patterns = ["*.vm.example.com"]
```

Every combination of `via` and connect route is allowed, because only your network topology determines which is correct. Resolving a name through the tunnel and then connecting to it directly is a valid setup when the address is reachable from this machine.

| Setting | Default | Description |
|---------|---------|-------------|
| `via` | `proxy` | Route carrying the DNS query (`proxy`, `direct`, `vm-direct`) |
| `servers` | (required) | DNS servers as `IP` or `IP:port`, tried in order. Must be IP addresses |
| `patterns` | (all hosts) | Hostname patterns this rule applies to |
| `timeout` | `3s` | Per-server query timeout |
| `min-ttl` | `10s` | Lower clamp on response TTL |
| `max-ttl` | `5m` | Upper clamp on response TTL |
| `negative-ttl` | `5s` | How long NXDOMAIN is cached |
| `on-failure` | `fallthrough` | `fallthrough` passes the hostname on unchanged; `fail` returns an error |
| `prefer` | `ipv4` | Address family preference (`ipv4` or `ipv6`) |

Notes:

- A `hosts` entry takes precedence over these rules, the way `/etc/hosts` takes precedence over a resolver.
- Destinations sent through an `upstream-proxy` keep their hostname, since the upstream proxy resolves them itself.
- When a `proxy` connection fails and falls back to another route, the fallback uses the original hostname rather than the resolved address, and the cached entry is dropped so the next attempt re-queries.
- Profile rules replace the rules from `defaults` rather than adding to them, since the list is an ordered decision table.

## CLI Reference

```bash
awsocks start [profile] [flags]
```

### Flags

| Flag | Description |
|------|-------------|
| `--aws-profile` | AWS profile name |
| `--region, -r` | AWS region |
| `--name, -n` | EC2 instance Name tag |
| `--instance-id, -i` | EC2 instance ID |
| `--ssh-key, -k` | Path to SSH private key |
| `--ssh-user, -u` | SSH username |
| `--listen, -l` | Listen address |
| `--auto-start` | Auto-start stopped EC2 instance |
| `--auto-stop` | Auto-stop EC2 instance on exit |
| `--route-default` | Default route (proxy/direct) |
| `--route-proxy` | Patterns to route via proxy |
| `--route-direct` | Patterns to route directly |
| `--dns-server` | DNS server IP for hostname resolution |
| `--lazy` | Enable lazy connection mode |
| `--proxy-network` | Proxy network (direct/vm) |
| `--idle-timeout` | Suspend EC2 after idle period |

## Prerequisites

### EC2 Instance

1. **SSM Agent** installed and running
2. **SSH server** with your public key in `~/.ssh/authorized_keys`

### IAM Permissions

The AWS profile needs these permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances"
      ],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ec2:StartInstances",
        "ec2:StopInstances"
      ],
      "Resource": "arn:aws:ec2:*:*:instance/*",
      "Condition": {
        "StringEquals": {
          "aws:ResourceTag/Name": "your-bastion-name"
        }
      }
    },
    {
      "Effect": "Allow",
      "Action": [
        "ssm:StartSession",
        "ssm:TerminateSession"
      ],
      "Resource": [
        "arn:aws:ec2:*:*:instance/*",
        "arn:aws:ssm:*:*:document/AWS-StartSSHSession"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "ssmmessages:CreateControlChannel",
        "ssmmessages:CreateDataChannel",
        "ssmmessages:OpenControlChannel",
        "ssmmessages:OpenDataChannel"
      ],
      "Resource": "*"
    }
  ]
}
```

**Note:** `ec2:StartInstances` and `ec2:StopInstances` are only required if using `--auto-start` or `--auto-stop` flags. Adjust the Condition to match your instance tags.

### EC2 Instance Role

The EC2 instance needs the `AmazonSSMManagedInstanceCore` managed policy.

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Browser   │────▶│   awsocks   │────▶│  SSM/SSH    │────▶│  Internal   │
│             │     │  (SOCKS5)   │     │  (EC2)      │     │  Services   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
                           │                   │
                           │                   ▼
                           │            ┌─────────────┐
                           │            │ VPC Private │
                           │            │    DNS      │
                           │            └─────────────┘
                           │
                    Domain-based
                     Routing
                           │
                           ▼
                    ┌─────────────┐
                    │   Direct    │
                    │ Connection  │
                    └─────────────┘
```

## License

GPL-2.0 License

This project includes Linux kernel and busybox in VM mode, which are licensed under GPL-2.0.
