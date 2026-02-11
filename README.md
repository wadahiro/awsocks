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
```

### Defaults

| Setting | Default | Description |
|---------|---------|-------------|
| `ssh-user` | `ec2-user` | SSH username on EC2 |
| `listen` | `127.0.0.1:1080` | SOCKS5 listen address |
| `lazy` | `true` | Defer AWS connection until first request |
| `mode` | `direct` | Execution mode (`direct` or `vm`) |

### Routing

Routes determine how traffic is handled:

| Route | Description |
|-------|-------------|
| `proxy` | Route through EC2 via SSM |
| `direct` | Direct connection from host (bypass proxy) |
| `vm-direct` | Direct from VM NAT (VM mode only) |

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
| `--lazy` | Enable lazy connection mode |
| `--mode, -m` | Execution mode (direct/vm) |

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
