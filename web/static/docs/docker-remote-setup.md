# Docker monitor — local & remote setup guide

Phoenix’s **Docker Container** monitor talks to a **Docker Engine API**. It does not use the Kubernetes API and does not talk to containerd directly.

You can point it at:

| Mode         | `docker_daemon` example              | Typical use                                              |
| ------------ | ------------------------------------ | -------------------------------------------------------- |
| Local socket | `unix:///var/run/docker.sock`        | Phoenix runs on the same machine as Docker               |
| Remote TCP   | `tcp://docker-host.example.com:2376` | Phoenix (e.g. in Kubernetes) checks a remote Docker host |

Config fields in the UI:

- **Docker host / API URL** → `docker_daemon`
- **Container Name/ID** → `container` (exact name or ID on that engine)

### Client URL vs daemon `hosts` (do not mix these up)

Phoenix and the `docker` CLI are **clients**. They only ever need a client-facing address:

| Put in Phoenix                | Meaning                                    |
| ----------------------------- | ------------------------------------------ |
| `unix:///var/run/docker.sock` | Local Engine via the Unix socket (default) |
| `tcp://host:2376`             | Remote Engine over TLS                     |
| `tcp://host:2375`             | Remote Engine plain TCP (lab only)         |

**Never put `fd://` in Phoenix.** That scheme is **not** a client URL.

On many Linux distros, **dockerd** is started by **systemd socket activation**. In `daemon.json` / unit config you may see:

```json
"hosts": ["fd://", "tcp://0.0.0.0:2376"]
```

| Scheme                        | Who uses it                      | Meaning                                                                                       |
| ----------------------------- | -------------------------------- | --------------------------------------------------------------------------------------------- |
| `unix:///var/run/docker.sock` | **Clients** (Phoenix, CLI)       | Open this path to talk to Docker                                                              |
| `fd://`                       | **dockerd only** (under systemd) | Inherit the already-opened socket from systemd; that socket is usually `/var/run/docker.sock` |
| `tcp://0.0.0.0:2376`          | **dockerd**                      | Also listen for remote TLS clients                                                            |

So: local clients still use `unix:///var/run/docker.sock` even when the daemon is configured with `fd://`. Re-adding `unix:///var/run/docker.sock` to `hosts` on a systemd-managed install can conflict with socket activation (“address already in use”). Prefer `fd://` on those hosts, or follow your distro’s Docker packaging docs.

---

## When this works

- The Docker **Engine API** must be reachable from the Phoenix process.
- The named container must exist on **that** engine.
- Firewall / security groups must allow Phoenix → Docker host on the API port.

## When this does **not** work (without extra setup)

| Environment                   | Why                                    |
| ----------------------------- | -------------------------------------- |
| Default Kubernetes pod        | No Docker socket; no host Docker API   |
| containerd / CRI-O only nodes | No classic Docker Engine API           |
| “Pod name” as container       | Docker API does not know K8s pod names |

For Kubernetes service health, prefer **HTTP**, **TCP**, **gRPC**, or **push** monitors against the Service/Ingress.

---

## Option A — Local Unix socket (same host)

1. Run Phoenix on a host (or VM) that has Docker Engine.
2. Ensure Phoenix can read the socket (user in `docker` group, or run as root — prefer least privilege).
3. Set:

```text
Docker host / API URL:  unix:///var/run/docker.sock
Container Name/ID:      my-container
```

4. Save and confirm the monitor goes **UP** when the container is running.

### Docker Compose / bind-mount (optional)

If Phoenix itself runs in a container on that host:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
```

**Security:** mounting the Docker socket is effectively root on the host. Prefer remote TLS or a dedicated monitoring host.

---

## Option B — Remote Docker API over TCP + TLS (recommended)

Expose Docker’s API only with TLS client authentication. Unencrypted `2375` is for labs only.

### 1. Generate TLS material on the Docker host

Follow Docker’s official protect-the-docker-daemon guide, or use a private CA. You need:

- CA certificate (`ca.pem`)
- Server certificate + key for the Docker host
- Client certificate + key for Phoenix (or an operator)

### 2. Configure Docker Engine (dockerd)

Example `daemon.json` on a **systemd** host (paths vary by distro).  
`fd://` keeps the local socket via systemd; `tcp://…:2376` adds the remote TLS listener.  
This is **server-side only** — Phoenix still uses `tcp://docker-host:2376`, not `fd://`.

```json
{
  "hosts": ["fd://", "tcp://0.0.0.0:2376"],
  "tls": true,
  "tlsverify": true,
  "tlscacert": "/etc/docker/certs/ca.pem",
  "tlscert": "/etc/docker/certs/server-cert.pem",
  "tlskey": "/etc/docker/certs/server-key.pem"
}
```

On hosts **without** systemd socket activation (e.g. some custom installs), you may instead use an explicit Unix host:

```json
{
  "hosts": ["unix:///var/run/docker.sock", "tcp://0.0.0.0:2376"],
  "tls": true,
  "tlsverify": true,
  "tlscacert": "/etc/docker/certs/ca.pem",
  "tlscert": "/etc/docker/certs/server-cert.pem",
  "tlskey": "/etc/docker/certs/server-key.pem"
}
```

Restart Docker. Confirm the API listens on **2376** (TLS), not open **2375** on the public internet. Local tools on that host still connect with `unix:///var/run/docker.sock`.

### 3. Firewall

Allow **only** Phoenix’s egress IP (or VPN/tailnet) to `docker-host:2376`.

### 4. Point Phoenix at the remote API

```text
Docker host / API URL:  tcp://docker-host.example.com:2376
Container Name/ID:      my-container
```

> **Note:** The current Phoenix Docker checker uses the host URL only. Full client-certificate TLS wiring in the UI may require env-based Docker client defaults (`DOCKER_CERT_PATH`, etc.) depending on deployment. Prefer placing Phoenix where mTLS is already configured for the Docker client, or use a network path (VPN + restricted API) until cert fields land in the form.

### 5. Verify from the Phoenix host

```bash
# From a machine that can reach the API (adjust certs):
docker --tlsverify \
  --tlscacert=ca.pem --tlscert=cert.pem --tlskey=key.pem \
  -H tcp://docker-host.example.com:2376 \
  ps
```

If `docker ps` works from that network path, Phoenix can use the same URL once client trust is in place.

---

## Option C — Lab only: TCP without TLS (do not use in production)

```bash
# Example: insecure listen (DANGEROUS on untrusted networks)
dockerd -H unix:///var/run/docker.sock -H tcp://0.0.0.0:2375
```

Phoenix:

```text
Docker host / API URL:  tcp://docker-host.example.com:2375
```

Anyone who can reach port 2375 can control the Docker host. Use only on isolated lab networks.

---

## Kubernetes: common patterns

| Goal                              | Approach                                                                                    |
| --------------------------------- | ------------------------------------------------------------------------------------------- |
| Check an app in the cluster       | HTTP/TCP/gRPC to Service DNS, not Docker                                                    |
| Check a **remote VM** Docker host | Option B from a Phoenix pod that can route to that host                                     |
| Check Docker on the **node**      | Not recommended (socket mount + privileges); use node agents or external Docker API instead |

---

## Troubleshooting

| Symptom                                                | Likely cause                                                                                           |
| ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------ |
| `failed to create Docker client`                       | Bad URL scheme/host, or daemon not listening                                                           |
| Error mentioning `fd://` in Phoenix                    | That value is daemon-only — use `unix:///var/run/docker.sock` or `tcp://host:port` in the UI           |
| Connection timeout                                     | Firewall, wrong port, Phoenix cannot route to host                                                     |
| Container not found                                    | Wrong name/ID, or wrong Docker host                                                                    |
| Permission denied (socket)                             | Phoenix user cannot access `/var/run/docker.sock`                                                      |
| TLS handshake error                                    | Missing/mismatched client certs or wrong port (2375 vs 2376)                                           |
| dockerd “address already in use” after editing `hosts` | Clash with systemd socket activation — use `fd://` instead of re-binding `unix:///var/run/docker.sock` |

---

## Security checklist

- [ ] Prefer TLS + client auth (`2376`) over plain TCP (`2375`)
- [ ] Restrict source IPs to Phoenix only
- [ ] Do not mount Docker socket into multi-tenant workloads unless required
- [ ] Use least-privilege network policies in Kubernetes
- [ ] Treat Docker API access as equivalent to root on that host

---

## Related

- Monitor type: `docker` (`internal/adapters/checker/docker.go`)
- Form field: **Docker host / API URL** (`docker_daemon`)
- Prefer other monitor types for pure application uptime when Docker API access is unavailable
