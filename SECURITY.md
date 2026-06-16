# Security Policy

## Trust model

Teamster is designed for **trusted LAN environments**. In v0.1.0, none of
its network services have authentication. All endpoints are open to the
network by design — this is intentional for a self-hosted tool running
on a private network.

Teamster's network services are centralized — remote hosts connect to the
hub over HTTP, so these services must be reachable on the LAN:

| Service | Default bind | Purpose |
|---------|-------------|---------|
| hookd | `0.0.0.0:9125` | Event receiver, dashboard, SSE stream, MCP routes |
| activity-mcp | HTTP via hookd `/mcp/activity` | Activity reporting |
| wms-mcp | HTTP via hookd `/mcp/wms` | Work management CRUD |

**You should:**
- Run all Teamster services on a private network only
- Use firewall rules to restrict access to the hookd port (default 9125)
  to trusted source IPs
- Never expose Teamster services to the public internet

Authentication and access control are planned for a future release.

## Supported versions

Only the latest release is supported with security updates.

| Version | Supported |
|---------|-----------|
| 0.1.x   | Yes       |

## Reporting a vulnerability

Report via [GitHub Security Advisories](https://github.com/bmjdotnet/teamster/security/advisories/new).
Include:

1. Description of the vulnerability
2. Steps to reproduce
3. Impact assessment (what an attacker could do)

**Response timeline:** We will acknowledge your report within 72 hours and
provide an initial assessment within one week.

Please do **not** open a public GitHub issue for security vulnerabilities.
Use the private advisory link above so we can coordinate a fix before
disclosure.

## Scope

**In scope:**
- Go binaries (`teamster`, `hookd`, `feed`, `rollup`, `classify`,
  `wms-mcp`, `activity-mcp`, `token-scraper`, `relay`)
- Python hook client (`teamster.py`)
- The installer (`install.sh`, `lib/installrunner.sh`, `teamster-install`)
- The Claude Code plugin (`skel/lib/plugin/`)

**Out of scope:**
- Grafana dashboard JSON files (standard Grafana provisioning, no custom code)
- Systemd unit templates (rendered from `.tmpl` files at install time)
- Third-party dependencies (report upstream, but let us know if it affects
  Teamster)

## Credential handling

Teamster stores a MySQL DSN containing database credentials in environment
variables and systemd unit files. The hook client and MCP servers read
credentials from environment variables only -- never from command-line
arguments or configuration files accessible to other users.

If you discover credentials being logged, transmitted in the clear beyond
the local network, or persisted somewhere unexpected, that is a reportable
vulnerability.
