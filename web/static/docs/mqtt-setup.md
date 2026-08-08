# MQTT monitor — setup guide

Uptime Phoenix’s **MQTT Broker** monitor connects to a broker, optionally **subscribes** to a topic, and marks the check **UP** when the connection (and optional payload match) succeeds.

It uses the Eclipse Paho MQTT client. There is **no separate “WebSocket path” field** — put the full broker address (including path for MQTT-over-WebSocket) in **Broker URL**.

---

## Form fields

| UI label            | Config key              | Required    | Notes                                                    |
| ------------------- | ----------------------- | ----------- | -------------------------------------------------------- |
| Broker URL          | `broker`                | Yes         | Full URL with scheme (see below)                         |
| Topic to Subscribe  | `topic`                 | Recommended | Default `#` if empty                                     |
| Success message     | `success_message`       | No          | If set, wait for a payload that **contains** this string |
| Username / Password | `username` / `password` | No          | Broker auth                                              |
| Timeout (s)         | `timeout`               | No          | Default `10`                                             |

---

## Broker URL schemes

| Scheme              | Transport                 | Typical port | Example                            |
| ------------------- | ------------------------- | ------------ | ---------------------------------- |
| `mqtt://`           | Plain TCP                 | 1883         | `mqtt://broker.internal:1883`      |
| `mqtts://`          | TCP + TLS                 | 8883         | `mqtts://broker.example.com:8883`  |
| `tcp://`            | Plain TCP (alias)         | 1883         | `tcp://10.0.0.5:1883`              |
| `ssl://` / `tls://` | TCP + TLS (alias)         | 8883         | `ssl://broker.example.com:8883`    |
| `ws://`             | MQTT over WebSocket       | 9001 / 8083  | `ws://broker.internal:9001/mqtt`   |
| `wss://`            | MQTT over WebSocket + TLS | 8084 / 443   | `wss://mqtt.example.com:8084/mqtt` |

### Native MQTT (most common)

```text
Broker URL:  mqtt://mosquitto.monitoring.svc:1883
Topic:       health/phoenix
```

Use this when Uptime Phoenix can reach the broker on the classic MQTT port (same VPC, cluster network, or host network).

### MQTT over WebSocket (custom path)

Many brokers expose MQTT on HTTP/WebSocket for browsers or reverse proxies. **Include the path in the URL** — Uptime Phoenix does not add `/mqtt` for you.

| Broker / product                | Example URL                                     |
| ------------------------------- | ----------------------------------------------- |
| Eclipse Mosquitto (ws listener) | `ws://mosquitto:9001/mqtt`                      |
| EMQX                            | `ws://emqx:8083/mqtt` or `wss://emqx:8084/mqtt` |
| HiveMQ Cloud                    | `wss://….s1.eu.hivemq.cloud:8884/mqtt`          |
| Behind nginx path               | `wss://mqtt.example.com/ws/mqtt`                |

```text
Broker URL:  wss://mqtt.example.com:8084/mqtt
Topic:       devices/+/status
Username:    monitor
Password:    ••••••••
```

**Wrong** (path missing when the broker requires it):

```text
wss://mqtt.example.com:8084
```

**Right** (path included):

```text
wss://mqtt.example.com:8084/mqtt
```

If you only have host + port + path from another tool (e.g. Uptime Kuma):

| Pieces                                                     | Build this URL                     |
| ---------------------------------------------------------- | ---------------------------------- |
| host `mqtt.example.com`, port `8084`, path `/mqtt`, TLS on | `wss://mqtt.example.com:8084/mqtt` |
| host `localhost`, port `9001`, path `/mqtt`, TLS off       | `ws://localhost:9001/mqtt`         |
| host `broker`, port `1883`, native MQTT                    | `mqtt://broker:1883`               |

---

## What the check does

1. Connect to the broker with a short-lived client id (`phoenix-check-…`).
2. Subscribe to **Topic** (default `#`).
3. If **Success message** is empty → **UP** as soon as connect + subscribe succeed.
4. If **Success message** is set → wait (until timeout) for a published message on that subscription whose payload **contains** the string; then **UP**. Otherwise **DOWN** (timeout).

Good uses for success message:

- LWT / status topics that publish `online` or `{"status":"up"}`
- A retained “heartbeat” payload your app publishes on a known topic

---

## Network & security checklist

- Uptime Phoenix must reach the broker **from the Uptime Phoenix process** (pod, VM, or host).
- Open firewall / security groups for the broker port (1883, 8883, 8083, 8084, 9001, …).
- For `mqtts://` / `wss://`, the certificate must be valid for the hostname Uptime Phoenix uses (or use a private CA trusted by the OS running Uptime Phoenix). There is currently **no** MQTT-specific “ignore TLS errors” toggle like HTTP’s `tls_ignore`.
- Prefer a dedicated broker user with subscribe-only rights on the topics you monitor.
- Do not put secrets only in the monitor **name** — use **Password** / broker ACLs.

---

## Kubernetes tips

| Goal                        | Approach                                                                                                                      |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Broker in the same cluster  | `mqtt://mosquitto.namespace.svc:1883` (or ClusterIP service DNS)                                                              |
| Broker only on host network | Run Uptime Phoenix where that network is reachable, or expose the broker via Service / Ingress carefully                             |
| Public WSS via Ingress      | Point `wss://your-domain/mqtt` (or your Ingress path) at the broker’s WebSocket listener; put that full URL in **Broker URL** |

MQTT-over-WebSocket behind Ingress needs **WebSocket upgrade** support (sticky sessions usually not required for a short check).

---

## Common failures

| Symptom                           | Likely cause                                                                                 |
| --------------------------------- | -------------------------------------------------------------------------------------------- |
| `mqtt connect failed`             | Wrong host/port, firewall, TLS scheme mismatch (`mqtt://` vs `mqtts://`)                     |
| Connect OK then subscribe timeout | ACL denies subscribe, or broker slow; increase timeout                                       |
| Message wait timeout              | No publisher on that topic, wrong topic filter, or success string does not appear in payload |
| Works in CLI, fails in Uptime Phoenix    | Uptime Phoenix runs elsewhere (different network namespace); test from the Uptime Phoenix pod/host         |
| WSS fails, TCP works              | Missing path (`/mqtt`), wrong WSS port, or TLS cert name mismatch                            |

---

## Related monitors

| Need                               | Prefer                       |
| ---------------------------------- | ---------------------------- |
| Only “is TCP port open?”           | **TCP** monitor on 1883/8883 |
| HTTP health of a broker admin API  | **HTTP(s)**                  |
| App-level “I’m alive” without MQTT | **Push** heartbeat           |

Use **MQTT** when you care that the **broker protocol** (and optionally a topic payload) works, not only that a port accepts connections.
