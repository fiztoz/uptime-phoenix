# RabbitMQ monitor — setup guide

Uptime Phoenix’s **RabbitMQ** monitor checks an **AMQP 0-9-1** broker (RabbitMQ and compatible servers). It connects, opens a channel, and optionally verifies that a **queue** or **exchange** still exists via **passive declare**.

It does **not** publish or consume messages. Prefer a dedicated least-privilege user (scripts below).

---

## Form fields

| UI label      | Config key      | Required | Notes                                                                  |
| ------------- | --------------- | -------- | ---------------------------------------------------------------------- |
| AMQP URL      | `url`           | Yes      | Full `amqp://` or `amqps://` URL (aliases: `connection_string`, `dsn`) |
| Queue name    | `queue`         | No       | Passive declare — DOWN if missing or no permission                     |
| Exchange name | `exchange`      | No       | Passive declare — type must match                                      |
| Exchange type | `exchange_type` | No       | Default `direct` (`topic` / `fanout` / `headers`)                      |
| Timeout (s)   | `timeout`       | No       | Default `10`                                                           |

Leave **both** queue and exchange empty to check **connect + open channel** only.

---

## What the check does

1. Dial and authenticate with the AMQP URL.
2. Open a channel.
3. If **Queue** is set → `QueueDeclarePassive` (queue must already exist).
4. Else if **Exchange** is set → `ExchangeDeclarePassive` with the chosen type.
5. Close channel and connection.

| Outcome                                | Meaning                                                 |
| -------------------------------------- | ------------------------------------------------------- |
| **UP** — connected and opened channel  | Broker accepted the user; channel works                 |
| **UP** — queue/exchange exists         | Resource is present and the user may passive-declare it |
| **DOWN** — connect failed              | Network, TLS, auth, or vhost                            |
| **DOWN** — queue/exchange check failed | Resource missing, wrong type, or ACL                    |

---

## AMQP URL formats

### Default vhost (`/`)

The default vhost is a single slash. In a URL it is often written as `%2F`:

```text
amqp://phoenix_monitor:CHANGE_ME@rabbitmq.example.com:5672/%2F
```

### Named vhost

```text
amqp://phoenix_monitor:CHANGE_ME@rabbitmq.example.com:5672/app
```

### TLS (AMQPS)

```text
amqps://phoenix_monitor:CHANGE_ME@rabbitmq.example.com:5671/%2F
```

Port **5671** is the usual AMQPS listener; port **5672** is plain AMQP.

### Credentials in the URL

```text
amqp://USERNAME:PASSWORD@HOST:PORT/VHOST
```

Use URL-encoding for special characters in password or vhost (`@` → `%40`, `/` → `%2F`, space → `%20`).

Uptime Phoenix stores the URL in monitor config (treated as a secret field in the form). Prefer a monitor-only password, not the broker admin.

---

## Create a least-privilege monitor user

**Do not** use the `guest` user (often localhost-only) or an admin account.

Scripts:

| Asset                       | Repo                                                             | Served URL                                                |
| --------------------------- | ---------------------------------------------------------------- | --------------------------------------------------------- |
| `rabbitmqctl` script        | `docs/scripts/rabbitmq-monitor/create-user-rabbitmq.sh`          | `/docs/rabbitmq-monitor/create-user-rabbitmq.sh`          |
| Definitions JSON (optional) | `docs/scripts/rabbitmq-monitor/phoenix-monitor-definitions.json` | `/docs/rabbitmq-monitor/phoenix-monitor-definitions.json` |

Also available in the UI: **Create/Edit monitor → RabbitMQ → View setup guide**.

### Option A — `rabbitmqctl` (recommended)

On a host that can run `rabbitmqctl` against the cluster (or inside the RabbitMQ container):

```bash
# Edit password / vhost / optional resource names in the script first, then:
bash docs/scripts/rabbitmq-monitor/create-user-rabbitmq.sh
```

Or run the equivalent by hand:

```bash
# === EDIT ===
USER=phoenix_monitor
PASS='CHANGE_ME_STRONG_PASSWORD'
VHOST=/                    # or app
QUEUE=                     # optional, e.g. health-check
EXCHANGE=                  # optional, e.g. amq.topic

rabbitmqctl add_user "$USER" "$PASS" 2>/dev/null || rabbitmqctl change_password "$USER" "$PASS"
rabbitmqctl set_user_tags "$USER" monitoring

# Connect + channel only (no queue/exchange check in Phoenix):
rabbitmqctl set_permissions -p "$VHOST" "$USER" "" "" ""

# If Phoenix will check a queue (passive declare needs configure on that name):
# rabbitmqctl set_permissions -p "$VHOST" "$USER" "^health-check$" "" ""

# If Phoenix will check an exchange:
# rabbitmqctl set_permissions -p "$VHOST" "$USER" "^amq\\.topic$" "" ""

# Queue AND exchange (configure regex matches both names):
# rabbitmqctl set_permissions -p "$VHOST" "$USER" "^(health-check|amq\\.topic)$" "" ""
```

**Permission model (RabbitMQ):**

| Permission                                 | Uptime Phoenix needs it for             |
| ------------------------------------------ | -------------------------------- |
| Login                                      | Always                           |
| configure / write / read empty             | Connect + open channel only      |
| **configure** matching queue/exchange name | Passive declare of that resource |

Uptime Phoenix does **not** need publish (`write`) or consume (`read`) for this monitor.

### Option B — Management definitions import

1. Edit `phoenix-monitor-definitions.json` (password, vhost, optional permissions).
2. Import via Management UI (**Import definitions**) or:

```bash
rabbitmqctl import_definitions /path/to/phoenix-monitor-definitions.json
```

### Verify outside Uptime Phoenix

```bash
# Connection (needs rabbitmqadmin or any AMQP client). Example with rabbitmq-diagnostics:
rabbitmqctl authenticate_user phoenix_monitor 'CHANGE_ME_STRONG_PASSWORD'

# Or from a machine with network access, use an AMQP client library / CLI of your choice
# against amqp://phoenix_monitor:…@HOST:5672/%2F
```

---

## Uptime Phoenix UI steps

1. Create the monitor user (script above).
2. **Monitors → Create → RabbitMQ** (under **Protocols**).
3. Set **AMQP URL**, e.g.  
   `amqp://phoenix_monitor:…@rabbitmq.monitoring.svc:5672/%2F`
4. Optional: set **Queue** or **Exchange** (+ type) if you want resource existence checks.
5. Save; wait one interval → expect **UP**.
6. Stop RabbitMQ or revoke permissions → expect **DOWN** with a clear message.

---

## Kubernetes tips

| Goal                   | Approach                                                                     |
| ---------------------- | ---------------------------------------------------------------------------- |
| Broker in-cluster      | Service DNS: `amqp://…@rabbitmq.namespace.svc:5672/%2F`                      |
| TLS to broker          | `amqps://` and port 5671 (or your TLS listener)                              |
| Only probe “broker up” | URL only — no queue/exchange                                                 |
| Probe a critical queue | Create durable queue; grant configure on that name; set **Queue** in Uptime Phoenix |

Ensure NetworkPolicies allow Uptime Phoenix pods → RabbitMQ AMQP port.

---

## Common failures

| Symptom                              | Likely cause                                                   |
| ------------------------------------ | -------------------------------------------------------------- |
| `connect failed` / timeout           | Wrong host/port, NetworkPolicy, broker down                    |
| Access refused / ACCESS_REFUSED      | Bad password, user missing, vhost wrong                        |
| guest login fails from non-localhost | Expected — use `phoenix_monitor`, not `guest`                  |
| Queue check failed                   | Queue does not exist, or user lacks **configure** on that name |
| Exchange check failed                | Wrong **exchange type**, missing exchange, or ACL              |
| Works on laptop, fails in Uptime Phoenix    | Different network path; allow Uptime Phoenix egress                   |

---

## Related monitors

| Need                       | Prefer                                                                           |
| -------------------------- | -------------------------------------------------------------------------------- |
| Only “port open”           | **TCP** on 5672/5671                                                             |
| Management HTTP API health | **HTTP(s)** to `/api/health/checks/alarms` (needs management plugin + user tags) |
| MQTT plugin on RabbitMQ    | **MQTT** monitor (different protocol)                                            |
| App publishes heartbeats   | **Push**                                                                         |

Use **RabbitMQ** when you care that AMQP auth and (optionally) a named queue/exchange still work.
