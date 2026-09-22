# wharf-agent

[![CI](https://github.com/forgelab-me/wharf-agent/actions/workflows/agent.yml/badge.svg)](https://github.com/forgelab-me/wharf-agent/actions/workflows/agent.yml)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Runs on each Docker host you want Wharf to manage. Enrolls with the controller over a self-signed, fingerprint-pinned mTLS connection ("connect first, approve later"), then polls for deploy commands and executes them against the local `docker.sock` — no long-lived shared credentials, no orchestrator, just `docker compose up`/`down` on the host it actually runs on. See [wharf-server](https://github.com/forgelab-me/wharf-server) for the controller this connects to.

## Building and running

Pull the published image, or build the same thing locally — no local Go toolchain needed either way:

```bash
docker build -t wharf-agent:latest .   # or: docker pull ghcr.io/forgelab-me/wharf-agent:latest
```

```bash
docker run -d \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /opt/wharf-agent/identity:/var/lib/wharf-agent/identity \
  -v /opt/wharf-agent/stacks:/opt/wharf-agent/stacks \
  ghcr.io/forgelab-me/wharf-agent:latest \
  --controller=https://<controller-host>:8443 \
  --controller-fingerprint=sha256:<fingerprint-from-the-controller-ui>
```

Both `/opt/wharf-agent/...` mounts must be bind mounts at that exact path on both sides (host and container), not named volumes — cf. the note on `/opt/wharf-agent/stacks` under Volumes below.

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--controller` | *(required)* | The controller's agent endpoint, e.g. `https://wharf.example.internal:8443` |
| `--controller-fingerprint` | *(none)* | Expected SHA-256 fingerprint of the controller's certificate. Strongly recommended — without it the agent trusts whatever certificate it sees on first connection (TOFU), fine for local testing, not for anything reachable over an untrusted network. |
| `--identity-dir` | `/var/lib/wharf-agent/identity` | Where this agent's own persistent TLS identity is kept |

### Volumes

- `/var/run/docker.sock` — required; how the agent talks to Docker on its host.
- `/var/lib/wharf-agent/identity` — **must persist across container recreation.** This is the agent's own enrollment identity; losing it means re-enrolling and re-approving from scratch.
- `/opt/wharf-agent/stacks` — where deployed stacks' compose files and `.env`/secret files land. **Must be a bind mount at this exact host path, not a named volume.** The agent runs `docker compose` from inside its own container but against the *host's* Docker daemon (Docker-outside-of-Docker, via the mounted socket) — a `secrets: <name>: file: ...` in a stack's compose file resolves to an absolute path from the agent's own filesystem view, and the daemon then needs that same path to exist on its own disk to bind-mount it. A named volume gives a path that only exists inside the agent's container, invisible to the daemon; the deploy fails with "bind source path does not exist."

## What it actually does

1. **Enrollment** — generates a persistent self-signed TLS identity on first run, registers with the controller, and waits (`pending`) until an admin approves it from the Hosts page.
2. **Live state push** — once approved, opens a persistent WebSocket tunnel and pushes periodic snapshots of containers/images/volumes/networks; the controller never has to poll it for routine state.
3. **On-demand commands** — the same tunnel answers one-off requests from the controller: restart/stop a container, tail logs, inspect, live stats, `docker system df`, and so on.
4. **Deploys** — polls `GET /agent/commands` for queued work. A local (non-Git) stack's compose content is handed over directly; a Git stack is cloned with a transient deploy credential (SSH key or HTTP token, never persisted beyond the clone). Secrets are decrypted by relaying ciphertext back to the controller's custodian — the agent itself never holds a private key.

## Layout

```
agent/
├── main.go          enrollment, command polling, local/Git deploy execution
├── tunnel.go         the persistent WebSocket connection and its command handlers
└── internal/
    └── identity/     this agent's own self-signed TLS identity
```

## Changelog

See [CHANGELOG.md](CHANGELOG.md).

## License

[MIT](LICENSE).
