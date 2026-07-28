// Phoenix scale/load harness.
//
// The target MUST be disposable: setup creates monitors and never deletes them.
// See tests/load/README.md for the isolated MariaDB + app recipe.

import http from "k6/http";
import ws from "k6/ws";
import { check, sleep } from "k6";
import { Counter, Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:3000";
const MONITOR_TARGET = __ENV.MONITOR_TARGET || `${BASE_URL}/api/health/live`;
const MONITOR_COUNT = Number.parseInt(__ENV.MONITOR_COUNT || "100", 10);
const API_VUS = Number.parseInt(__ENV.API_VUS || "5", 10);
const WS_CLIENTS = Number.parseInt(__ENV.WS_CLIENTS || "10", 10);
// Check interval for the seeded monitors, in seconds.
//
// This is parameterised because the default 60 s made the WS event-latency
// threshold unmeasurable: with a 30 s scenario, the whole window can fall
// BETWEEN two check rounds, so zero heartbeat events are sampled and
// ws_event_latency reports a meaningless p95 of 0. Setting it below the scenario
// duration guarantees every monitor checks at least once while the WebSocket
// clients are connected, which is the only way the fan-out threshold means
// anything. Default stays 60 so previously recorded runs remain reproducible.
const MONITOR_INTERVAL = Number.parseInt(__ENV.MONITOR_INTERVAL || "60", 10);
const TEST_DURATION = __ENV.TEST_DURATION || "30s";
const WS_HOLD_MS = Number.parseInt(__ENV.WS_HOLD_MS || "30000", 10);
const WS_MAX_DURATION = __ENV.WS_MAX_DURATION || "45s";

const apiResponseTime = new Trend("api_response_time", true);
const wsConnectTime = new Trend("ws_connect_time", true);
const wsEventLatency = new Trend("ws_event_latency", true);
const wsMessages = new Counter("ws_messages");
const wsHeartbeatMessages = new Counter("ws_heartbeat_messages");

export const options = {
  setupTimeout: "15m",
  batch: 50,
  batchPerHost: 50,
  scenarios: {
    api: {
      executor: "constant-vus",
      exec: "apiLoad",
      vus: API_VUS,
      duration: TEST_DURATION,
    },
    websocket: {
      executor: "per-vu-iterations",
      exec: "websocketClient",
      vus: WS_CLIENTS,
      iterations: 1,
      maxDuration: WS_MAX_DURATION,
    },
  },
  thresholds: {
    api_response_time: ["p(95)<500", "p(99)<1000"],
    http_req_failed: ["rate<0.01"],
    ws_connect_time: ["p(95)<1000"],
    ws_event_latency: ["p(95)<1000", "p(99)<2000"],
  },
};

function jsonHeaders(token) {
  return {
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
  };
}

function login() {
  if (__ENV.AUTH_TOKEN) return __ENV.AUTH_TOKEN;
  const response = http.post(
    `${BASE_URL}/api/auth/login`,
    JSON.stringify({
      username: __ENV.ADMIN_USERNAME || "admin",
      password: __ENV.ADMIN_PASSWORD || "ChangeMe123!",
    }),
    { headers: { "Content-Type": "application/json" } },
  );
  if (response.status !== 200) {
    throw new Error(`login failed: ${response.status} ${response.body}`);
  }
  const token = response.json("token");
  if (typeof token !== "string" || token.length === 0) {
    throw new Error("login response did not contain a JWT token");
  }
  return token;
}

function createMonitorRequest(token, index) {
  return {
    method: "POST",
    url: `${BASE_URL}/api/monitors`,
    body: JSON.stringify({
      name: `load-monitor-${String(index).padStart(5, "0")}`,
      type: "http",
      active: true,
      interval: MONITOR_INTERVAL,
      timeout: 5,
      retry_interval: 5,
      max_retries: 0,
      resend_interval: 0,
      tls_ignore: false,
      accepted_statuscodes: ["200-299"],
      config: { url: MONITOR_TARGET, method: "GET" },
    }),
    params: jsonHeaders(token),
  };
}

function seedMonitors(token) {
  const response = http.get(`${BASE_URL}/api/monitors`, jsonHeaders(token));
  if (response.status !== 200) {
    throw new Error(
      `monitor inventory failed: ${response.status} ${response.body}`,
    );
  }
  const existing = response.json();
  if (!Array.isArray(existing)) {
    throw new Error("monitor inventory was not an array");
  }

  for (let start = existing.length; start < MONITOR_COUNT; start += 50) {
    const requests = [];
    const end = Math.min(start + 50, MONITOR_COUNT);
    for (let index = start; index < end; index += 1) {
      requests.push(createMonitorRequest(token, index));
    }
    const responses = http.batch(requests);
    for (const created of responses) {
      if (created.status !== 201) {
        throw new Error(
          `monitor creation failed: ${created.status} ${created.body}`,
        );
      }
    }
    // Match Phoenix's default 50 request/s ingress limit instead of turning
    // fixture creation itself into an accidental rate-limit benchmark.
    sleep(1);
  }
  sleep(1);
}

export function setup() {
  if (!Number.isInteger(MONITOR_COUNT) || MONITOR_COUNT < 1) {
    throw new Error("MONITOR_COUNT must be a positive integer");
  }
  const token = login();
  seedMonitors(token);
  return { token };
}

export function apiLoad(data) {
  const response = http.get(
    `${BASE_URL}/api/monitors`,
    jsonHeaders(data.token),
  );
  apiResponseTime.add(response.timings.duration);
  if (response.status !== 200) {
    console.warn(`monitor inventory returned HTTP ${response.status}`);
  }
  check(response, { "monitor inventory is 200": (res) => res.status === 200 });
  sleep(1);
}

export function websocketClient(data) {
  const wsURL = `${BASE_URL.replace(/^http/, "ws")}/ws?token=${encodeURIComponent(data.token)}`;
  const startedAt = Date.now();
  const response = ws.connect(
    wsURL,
    { headers: { Origin: BASE_URL } },
    (socket) => {
      socket.on("open", () => wsConnectTime.add(Date.now() - startedAt));
      socket.on("message", (message) => {
        wsMessages.add(1);
        if (typeof message !== "string") return;
        let event;
        try {
          event = JSON.parse(message);
        } catch {
          return;
        }
        if (event.type !== "heartbeat") return;
        wsHeartbeatMessages.add(1);
        const recordedAt = Date.parse(event.payload?.time);
        if (Number.isFinite(recordedAt)) {
          wsEventLatency.add(Math.max(0, Date.now() - recordedAt));
        }
      });
      socket.setTimeout(() => socket.close(), WS_HOLD_MS);
    },
  );
  check(response, { "websocket upgraded": (res) => res?.status === 101 });
}
