/**
 * Per-monitor-type config field definitions for dynamic forms.
 * Used by MonitorForm to render conditional inputs.
 */
export type FieldType =
  | "text"
  | "number"
  | "select"
  | "textarea"
  | "json"
  | "checkbox"
  | "password";

export interface ConfigField {
  key: string;
  label: string;
  type: FieldType;
  required?: boolean;
  placeholder?: string;
  default?: string | number | boolean;
  options?: { value: string; label: string }[];
  help?: string;
  /** Optional form section ID declared by MonitorTypeMeta.sections. */
  section?: string;
  /** Span the full form width even when the control is a single-line input. */
  fullWidth?: boolean;
  /** Use tabular monospace styling for paths, payloads, and other exact data. */
  monospace?: boolean;
  /**
   * When set, the field is shown only if config[showWhen.key] is one of
   * showWhen.values (string compare). Used for auth sub-fields.
   */
  showWhen?: { key: string; values: string[] };
  /** Inclusive lower bound for type "number" inputs. */
  min?: number;
  /** Inclusive upper bound for type "number" inputs. */
  max?: number;
  /** Step increment for type "number" inputs. */
  step?: number;
}

/** High-level category for the create-monitor type picker (Kuma-style groups). */
export type MonitorTypeGroupId =
  | "general"
  | "passive"
  | "infrastructure"
  | "protocol";

export interface MonitorTypeMeta {
  label: string;
  /** One-line blurb shown under the label in the type picker. */
  description: string;
  /** Optional groups that turn long dynamic forms into scannable sections. */
  sections?: { id: string; label: string; description?: string }[];
  fields: ConfigField[];
}

/**
 * Ordered groups for the type select. Types listed here must exist in
 * `monitorTypeConfig`. Keep in lockstep with AGENTS.md's monitor-type list
 * (13 types).
 */
export const MONITOR_TYPE_GROUPS: ReadonlyArray<{
  id: MonitorTypeGroupId;
  /** Fallback English heading — prefer i18n in the UI. */
  label: string;
  types: readonly string[];
}> = [
  {
    id: "general",
    label: "General",
    types: ["http", "tcp", "ping", "dns", "websocket"],
  },
  {
    id: "passive",
    label: "Passive",
    types: ["push"],
  },
  {
    id: "infrastructure",
    label: "Infrastructure",
    types: ["docker", "database", "s3"],
  },
  {
    id: "protocol",
    label: "Protocols",
    types: ["mqtt", "rabbitmq", "grpc", "snmp"],
  },
];

/**
 * Canonical heartbeat domain status. Matches internal/core/domain/status.go
 * (StatusDown/StatusUp/StatusPending/StatusMaintenance) as surfaced by the REST
 * API's heartbeatView JSON (internal/adapters/http/handlers/heartbeat.go).
 *
 * Note: a Monitor's own display state ("paused") is a distinct, frontend-only
 * concept (derived from `active === false`) and is NOT part of this union —
 * see web/src/lib/stores/ws.svelte.ts's `Monitor["status"]`.
 */
export type Status = "up" | "down" | "pending" | "maintenance";

/**
 * Canonical heartbeat record shape, matching the REST API's `heartbeatView`
 * JSON (internal/adapters/http/handlers/heartbeat.go): id, monitor_id, status,
 * ping, message, time, important.
 */
export interface Heartbeat {
  id: number;
  monitor_id: number;
  status: Status;
  ping: number;
  message: string;
  time: string;
  important: boolean;
}

export const monitorTypeConfig: Record<string, MonitorTypeMeta> = {
  http: {
    label: "HTTP(s)",
    description: "Website or API — status, keyword, or JSON path",
    sections: [
      {
        id: "request",
        label: "Request",
        description: "Where to send the check and what to include.",
      },
      {
        id: "success",
        label: "Success criteria",
        description:
          "Every configured rule must pass for the monitor to stay up.",
      },
      {
        id: "authentication",
        label: "Authentication",
        description: "Credentials used only for this monitor request.",
      },
      {
        id: "diagnostics",
        label: "Diagnostic context",
        description:
          "Optionally attach response content to heartbeat history and notifications.",
      },
    ],
    fields: [
      {
        key: "url",
        label: "URL",
        type: "text",
        required: true,
        placeholder: "https://example.com/health",
        section: "request",
        fullWidth: true,
      },
      {
        key: "method",
        label: "Method",
        type: "select",
        default: "GET",
        section: "request",
        options: [
          { value: "GET", label: "GET" },
          { value: "POST", label: "POST" },
          { value: "HEAD", label: "HEAD" },
        ],
      },
      {
        key: "accepted_statuscodes",
        label: "Accepted Status Codes",
        type: "text",
        placeholder: "200-299,301",
        help: "Enter comma-separated codes or ranges. Defaults to any 2xx response.",
        section: "success",
      },
      {
        key: "keyword",
        label: "Keyword",
        type: "text",
        placeholder: "OK",
        help: "Optional text that must appear anywhere in the response body.",
        section: "success",
      },
      {
        key: "headers",
        label: "Headers",
        type: "textarea",
        placeholder: "X-Custom-Header: value",
        help: 'One "Key: Value" per line, or paste a JSON object. Prefer Authentication below for Basic/Bearer/OAuth2.',
        section: "request",
        monospace: true,
      },
      {
        key: "body",
        label: "Body",
        type: "textarea",
        placeholder: '{"foo":"bar"}',
        help: "Sent as the request body",
        section: "request",
        monospace: true,
      },
      {
        key: "json_query_syntax",
        label: "JSON path syntax",
        type: "select",
        default: "gjson",
        help: "JSONPath input is translated to GJSON when the monitor runs.",
        section: "success",
        options: [
          { value: "gjson", label: "GJSON" },
          { value: "jsonpath", label: "JSONPath ($...)" },
        ],
      },
      {
        key: "json_query",
        label: "JSON path",
        type: "text",
        placeholder: "data.status",
        help: 'GJSON example: status.conditions.#(type=="Ready").status. JSONPath supports $, child names, array indexes, and [*].',
        section: "success",
        fullWidth: true,
        monospace: true,
      },
      {
        key: "json_operator",
        label: "JSON condition",
        type: "select",
        default: "exists",
        section: "success",
        options: [
          { value: "exists", label: "Path exists" },
          { value: "has_value", label: "Has a value" },
          { value: "not_exists", label: "Path does not exist" },
          { value: "equals", label: "Value equals" },
          { value: "not_equals", label: "Value does not equal" },
          { value: "contains", label: "Value contains text" },
          { value: "not_contains", label: "Value does not contain text" },
        ],
      },
      {
        key: "expected_value",
        label: "Expected value",
        type: "text",
        required: true,
        placeholder: "True",
        help: 'Values are case-sensitive. Kubernetes condition status uses the string "True".',
        section: "success",
        monospace: true,
        showWhen: {
          key: "json_operator",
          values: ["equals", "not_equals", "contains", "not_contains"],
        },
      },
      {
        key: "follow_redirects",
        label: "Follow redirects",
        type: "checkbox",
        default: true,
        section: "request",
      },
      {
        key: "timeout",
        label: "Request timeout (s)",
        type: "number",
        default: 10,
        section: "request",
      },
      // Authentication (no NTLM / mTLS)
      {
        key: "auth_method",
        label: "Authentication",
        type: "select",
        default: "none",
        section: "authentication",
        options: [
          { value: "none", label: "None" },
          { value: "basic", label: "HTTP Basic Auth" },
          { value: "bearer", label: "Bearer Token" },
          { value: "oauth2_cc", label: "OAuth2: Client Credentials" },
        ],
        help: "Applied after custom headers (overwrites Authorization when set).",
      },
      {
        key: "auth_username",
        label: "Username",
        type: "text",
        placeholder: "user",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["basic"] },
      },
      {
        key: "auth_password",
        label: "Password",
        type: "password",
        placeholder: "••••••••",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["basic"] },
      },
      {
        key: "auth_bearer_token",
        label: "Bearer Token",
        type: "password",
        placeholder: "token",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["bearer"] },
      },
      {
        key: "oauth2_token_url",
        label: "Token URL",
        type: "text",
        placeholder: "https://auth.example.com/oauth/token",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["oauth2_cc"] },
      },
      {
        key: "oauth2_client_id",
        label: "Client ID",
        type: "text",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["oauth2_cc"] },
      },
      {
        key: "oauth2_client_secret",
        label: "Client Secret",
        type: "password",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["oauth2_cc"] },
      },
      {
        key: "oauth2_scopes",
        label: "Scopes (optional)",
        type: "text",
        placeholder: "read write",
        help: "Space-separated scopes sent with the token request",
        section: "authentication",
        showWhen: { key: "auth_method", values: ["oauth2_cc"] },
      },
      // Response body → notifications / heartbeat history
      {
        key: "save_body_on_error",
        label: "Save HTTP error body for notifications",
        type: "checkbox",
        default: false,
        help: "Appends a truncated response body when the check is DOWN (alerts + history).",
        section: "diagnostics",
      },
      {
        key: "save_body_on_success",
        label: "Save HTTP success body for notifications",
        type: "checkbox",
        default: false,
        help: "Appends a truncated response body when the check is UP (recovery alerts + history).",
        section: "diagnostics",
      },
    ],
  },
  tcp: {
    label: "TCP Port",
    description: "Open a TCP connection to host:port",
    fields: [
      {
        key: "hostname",
        label: "Hostname",
        type: "text",
        required: true,
        placeholder: "db.example.com",
      },
      {
        key: "port",
        label: "Port",
        type: "number",
        required: true,
        default: 5432,
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  ping: {
    label: "ICMP Ping",
    description: "Host reachability via ICMP echo",
    fields: [
      {
        key: "hostname",
        label: "Hostname / IP",
        type: "text",
        required: true,
        placeholder: "8.8.8.8",
      },
      { key: "count", label: "Ping Count", type: "number", default: 3 },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  dns: {
    label: "DNS Query",
    description: "Resolve a record and optionally assert value",
    fields: [
      {
        key: "hostname",
        label: "Hostname",
        type: "text",
        required: true,
        placeholder: "example.com",
      },
      {
        key: "resolve_type",
        label: "Record Type",
        type: "select",
        default: "A",
        options: [
          { value: "A", label: "A" },
          { value: "AAAA", label: "AAAA" },
          { value: "CNAME", label: "CNAME" },
          { value: "MX", label: "MX" },
          { value: "TXT", label: "TXT" },
        ],
      },
      {
        key: "resolve_server",
        label: "DNS Server",
        type: "text",
        default: "8.8.8.8",
      },
      {
        key: "expected_value",
        label: "Expected Value",
        type: "text",
        placeholder: "optional",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  websocket: {
    label: "WebSocket",
    description: "Upgrade and open a WebSocket connection",
    fields: [
      {
        key: "url",
        label: "WS URL",
        type: "text",
        required: true,
        placeholder: "wss://example.com/socket",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  push: {
    label: "Push (Heartbeat)",
    description: "Your app posts heartbeats to Phoenix",
    fields: [
      {
        key: "push_token",
        label: "Push Token (auto-generated on save)",
        type: "text",
        required: false,
        help: "Clients POST to /api/push/:token",
      },
    ],
  },
  docker: {
    label: "Docker Container",
    description:
      "Inspect a container via the Docker API (local socket or remote host)",
    fields: [
      {
        key: "docker_daemon",
        label: "Docker host / API URL",
        type: "text",
        default: "unix:///var/run/docker.sock",
        placeholder: "unix:///var/run/docker.sock or tcp://docker-host:2376",
        help: "Local Unix socket, or remote Docker Engine API (tcp://host:port). See remote Docker setup guide.",
      },
      {
        key: "container",
        label: "Container Name/ID",
        type: "text",
        required: true,
        placeholder: "nginx",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  mqtt: {
    label: "MQTT Broker",
    description: "Connect and subscribe to a topic",
    fields: [
      {
        key: "broker",
        label: "Broker URL",
        type: "text",
        required: true,
        placeholder: "mqtt://broker:1883 or wss://host:8084/mqtt",
        help: "Full URL including scheme. For MQTT over WebSocket, include the path (e.g. /mqtt) in the URL — there is no separate path field. See the setup guide.",
      },
      {
        key: "topic",
        label: "Topic to Subscribe",
        type: "text",
        required: true,
        placeholder: "health/status or devices/+/lwt",
        help: "Default # if left empty on the server. Use a specific topic when possible.",
      },
      {
        key: "success_message",
        label: "Success message (optional)",
        type: "text",
        placeholder: "online",
        help: "If set, the check waits for a payload that contains this string. If empty, connect + subscribe is enough for UP.",
      },
      { key: "username", label: "Username", type: "text" },
      { key: "password", label: "Password", type: "password" },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  rabbitmq: {
    label: "RabbitMQ",
    description: "AMQP broker connection and optional queue/exchange probe",
    fields: [
      {
        key: "url",
        label: "AMQP URL",
        type: "password",
        required: true,
        placeholder: "amqp://monitor:secret@rabbitmq:5672/%2F",
        help: "Use amqp:// or amqps:// (default vhost often %2F). Prefer a dedicated least-privilege user — see the setup guide for rabbitmqctl scripts.",
      },
      {
        key: "queue",
        label: "Queue name (optional)",
        type: "text",
        placeholder: "health-check",
        help: "If set, Phoenix passively declares this queue and marks the check DOWN if it does not exist or the user lacks permission.",
      },
      {
        key: "exchange",
        label: "Exchange name (optional)",
        type: "text",
        placeholder: "amq.topic",
        help: "If set, Phoenix passively declares this exchange. Leave both queue and exchange empty to check connection + channel open only.",
      },
      {
        key: "exchange_type",
        label: "Exchange type",
        type: "select",
        default: "direct",
        options: [
          { value: "direct", label: "direct" },
          { value: "topic", label: "topic" },
          { value: "fanout", label: "fanout" },
          { value: "headers", label: "headers" },
        ],
        help: "Only used when Exchange name is set; RabbitMQ requires the declared type to match.",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  grpc: {
    label: "gRPC Health",
    description: "gRPC health-check protocol probe",
    fields: [
      {
        key: "hostname",
        label: "Hostname:Port",
        type: "text",
        required: true,
        placeholder: "grpc.example.com:50051",
      },
      {
        key: "service",
        label: "Service Name",
        type: "text",
        placeholder: "optional",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  snmp: {
    label: "SNMP",
    description: "SNMP GET for a device OID",
    fields: [
      { key: "hostname", label: "Hostname", type: "text", required: true },
      { key: "port", label: "Port", type: "number", default: 161 },
      { key: "community", label: "Community", type: "text", default: "public" },
      {
        key: "oid",
        label: "OID to Query",
        type: "text",
        required: true,
        placeholder: "1.3.6.1.2.1.1.1.0",
      },
      { key: "timeout", label: "Timeout (s)", type: "number", default: 10 },
    ],
  },
  database: {
    label: "Database",
    description: "Connect ping (Postgres, MySQL, MariaDB, Mongo, Redis, MSSQL)",
    sections: [
      {
        id: "connection",
        label: "Connection",
        description: "Which engine to speak and how to connect.",
      },
      {
        id: "health",
        label: "Health check",
        description:
          "Presets only — connect ping, or a fixed SELECT 1 / PING. There is no query textbox.",
      },
      {
        id: "capacity",
        label: "Capacity alerts",
        description:
          "Optional checks that run after a successful connect. They use fixed server queries (never operator SQL). Thresholds and privilege errors become visible capacity conditions while availability stays UP. See the setup guide before enabling these in production.",
      },
    ],
    fields: [
      {
        key: "engine",
        label: "Engine",
        type: "select",
        required: true,
        default: "postgres",
        section: "connection",
        options: [
          { value: "postgres", label: "PostgreSQL" },
          { value: "mysql", label: "MySQL" },
          { value: "mariadb", label: "MariaDB" },
          { value: "mssql", label: "SQL Server (MSSQL)" },
          { value: "mongodb", label: "MongoDB" },
          { value: "redis", label: "Redis" },
        ],
        help: "Which database engine to speak. See the setup guide for DSN formats and a least-privilege user script.",
      },
      {
        key: "connection_string",
        label: "Connection string / DSN",
        type: "password",
        required: true,
        placeholder:
          "postgres://phoenix_monitor:…@host:5432/app?sslmode=require",
        section: "connection",
        fullWidth: true,
        help: "Use a dedicated read-only monitor user. Formats differ by engine — open the setup guide for examples.",
      },
      {
        key: "health_check",
        label: "Health check",
        type: "select",
        default: "ping",
        section: "health",
        options: [
          { value: "ping", label: "Connect + protocol ping only" },
          { value: "select_1", label: "Also run fixed SELECT 1 / PING" },
        ],
        help: "Presets only — no free-form SQL (avoids injection). SELECT 1 for SQL engines; PING for Redis/Mongo.",
      },
      {
        key: "timeout",
        label: "Timeout (s)",
        type: "number",
        default: 10,
        section: "health",
      },
      {
        key: "check_session_pool",
        label: "Check session pool",
        type: "checkbox",
        default: false,
        section: "capacity",
        help: "After a successful connect, measure current vs max connections. Over-threshold becomes a warning chip after two samples; connectivity remains UP. Extra grants may be required — see the setup guide. Prefer a 30s+ interval.",
      },
      {
        key: "session_pool_threshold",
        label: "Session pool threshold (%)",
        type: "number",
        default: 80,
        section: "capacity",
        min: 1,
        max: 100,
        step: 1,
        showWhen: { key: "check_session_pool", values: ["true"] },
        help: "Percent of max connections. Alert when used/max ≥ this value (1–100).",
      },
      {
        key: "check_storage",
        label: "Check storage",
        type: "checkbox",
        default: false,
        section: "capacity",
        help: "After a successful connect, measure used storage via a fixed engine query (never operator SQL). Over-threshold becomes a warning chip after two samples; connectivity remains UP. Extra grants may be required — see the setup guide. Prefer a 30s+ interval.",
      },
      {
        key: "storage_scope",
        label: "Storage scope",
        type: "select",
        default: "database",
        section: "capacity",
        options: [
          { value: "database", label: "This database only" },
          { value: "instance", label: "All databases on the instance" },
        ],
        showWhen: { key: "check_storage", values: ["true"] },
        help: "PostgreSQL and MySQL: measure the connected database, or sum every non-template / visible database on the instance. Compare Max size (GiB) to that same allocation — this is database data size, not host disk (WAL, logs, temp, and backups are excluded). MariaDB DISKS stays volume-level; if DISKS is unavailable the fallback follows this setting. Redis, MongoDB, and SQL Server keep their existing measurement.",
      },
      {
        key: "storage_threshold",
        label: "Storage threshold (%)",
        type: "number",
        default: 80,
        section: "capacity",
        min: 1,
        max: 100,
        step: 1,
        showWhen: { key: "check_storage", values: ["true"] },
        help: "Percent used. Alert when used/capacity ≥ this value (1–100).",
      },
      {
        key: "storage_max_gb",
        label: "Max size (GiB)",
        type: "number",
        section: "capacity",
        min: 0.1,
        step: 0.1,
        showWhen: { key: "check_storage", values: ["true"] },
        help: "Capacity in GiB used when the engine cannot report a total (typical PostgreSQL and MySQL). Match this to the storage scope: one database, or the instance allocation you compare against. Optional if Mongo fsTotalSize, MariaDB DISKS, SQL Server file size, or Redis maxmemory is available. Redis storage check is memory (used_memory), not disk.",
      },
    ],
  },
  s3: {
    label: "S3 / Object storage",
    description:
      "Signed HeadBucket or canary object — AWS, MinIO, S3-compatible",
    sections: [
      {
        id: "connection",
        label: "Connection",
        description:
          "Where to send the signed request. Leave endpoint empty for AWS.",
      },
      {
        id: "target",
        label: "Bucket",
        description:
          "Hyphens and underscores are allowed. Underscore names always use path-style addressing.",
      },
      {
        id: "authentication",
        label: "Authentication",
        description: "Dedicated read-only key. Never use the root account.",
      },
      {
        id: "health",
        label: "Health check",
        description:
          "Presets only — HeadBucket, HeadObject, or a small GetObject. No usage or quota probe.",
      },
    ],
    fields: [
      {
        key: "provider",
        label: "Provider",
        type: "select",
        default: "generic",
        section: "connection",
        options: [
          { value: "aws", label: "AWS S3" },
          { value: "minio", label: "MinIO" },
          { value: "generic", label: "S3-compatible (R2, Wasabi, Garage, …)" },
        ],
        help: "Hint for defaults only. All three speak the S3 REST API.",
      },
      {
        key: "endpoint",
        label: "Endpoint",
        type: "text",
        placeholder: "http://minio.minio.svc:9000",
        section: "connection",
        fullWidth: true,
        help: "Leave empty for AWS (s3.<region>.amazonaws.com). For MinIO include the scheme, e.g. http://minio:9000.",
      },
      {
        key: "region",
        label: "Region",
        type: "text",
        default: "us-east-1",
        section: "connection",
        help: "SigV4 region. MinIO commonly uses us-east-1.",
      },
      {
        key: "path_style",
        label: "Path-style addressing",
        type: "checkbox",
        default: true,
        section: "connection",
        help: "Use /bucket/key instead of bucket.host/key. Required for bucket names with '_' and for most MinIO / Garage / R2 endpoints. Hyphenated names work either way.",
      },
      {
        key: "bucket",
        label: "Bucket",
        type: "text",
        required: true,
        placeholder: "my-backup_bucket",
        section: "target",
        fullWidth: true,
        monospace: true,
        help: "3–63 characters. Letters, digits, '-', '_' and '.' are allowed.",
      },
      {
        key: "access_key",
        label: "Access key",
        type: "password",
        required: true,
        section: "authentication",
      },
      {
        key: "secret_key",
        label: "Secret key",
        type: "password",
        required: true,
        section: "authentication",
      },
      {
        key: "session_token",
        label: "Session token (optional)",
        type: "password",
        section: "authentication",
        help: "STS session token if you are pasting temporary credentials.",
      },
      {
        key: "health_check",
        label: "Health check",
        type: "select",
        default: "head_bucket",
        section: "health",
        options: [
          {
            value: "head_bucket",
            label: "HeadBucket — bucket exists and is reachable",
          },
          { value: "head_object", label: "HeadObject — canary object exists" },
          {
            value: "get_object",
            label: "GetObject — read a small canary object",
          },
        ],
        help: "Presets only. There is no storage-usage check — the S3 API cannot report quota cheaply.",
      },
      {
        key: "object_key",
        label: "Object key",
        type: "text",
        placeholder: "phoenix-canary",
        section: "health",
        fullWidth: true,
        monospace: true,
        showWhen: {
          key: "health_check",
          values: ["head_object", "get_object"],
        },
        help: "Required for HeadObject / GetObject. Use a dedicated canary object, not a data-lake prefix.",
      },
      {
        key: "timeout",
        label: "Timeout (s)",
        type: "number",
        default: 10,
        section: "health",
      },
    ],
  },
};

/** Flat list of type keys — derived so order follows group order (picker order). */
export const monitorTypes: string[] = MONITOR_TYPE_GROUPS.flatMap((g) => [
  ...g.types,
]);

/**
 * Build Select-friendly groups with labels + optional short descriptions.
 * Pass `groupLabel` to localize section headings.
 */
export function buildMonitorTypeSelectGroups(
  groupLabel?: (id: MonitorTypeGroupId) => string,
): {
  label: string;
  options: { value: string; label: string; description?: string }[];
}[] {
  return MONITOR_TYPE_GROUPS.map((group) => ({
    label: groupLabel?.(group.id) ?? group.label,
    options: group.types.map((type) => {
      const meta = monitorTypeConfig[type];
      return {
        value: type,
        label: meta?.label ?? type,
        description: meta?.description,
      };
    }),
  }));
}

/** Add an explicit secure scheme to a bare HTTP monitor target. */
export function normalizeHttpUrl(value: unknown): string {
  const url = String(value ?? "").trim();
  if (!url) return "";
  if (url.startsWith("//")) return `https:${url}`;
  return url.includes("://") ? url : `https://${url}`;
}

/**
 * The HTTP checker (internal/adapters/checker/http.go, `config["headers"]`) reads
 * headers as a JSON object with string values — e.g. {"Authorization": "Bearer xyz"}.
 * The form edits headers as human-friendly text (either "Key: Value" lines or a raw
 * JSON object); these helpers convert between that editable text and the object shape
 * the backend expects.
 */
export function parseHeadersInput(
  text: string,
): Record<string, string> | undefined {
  const trimmed = text.trim();
  if (!trimmed) return undefined;

  // Prefer a raw JSON object if the text parses as one.
  try {
    const parsed = JSON.parse(trimmed);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const out: Record<string, string> = {};
      for (const [key, value] of Object.entries(
        parsed as Record<string, unknown>,
      )) {
        if (key) out[key] = String(value);
      }
      return Object.keys(out).length > 0 ? out : undefined;
    }
  } catch {
    // Not JSON — fall through to "Key: Value" line parsing.
  }

  const out: Record<string, string> = {};
  for (const line of trimmed.split("\n")) {
    const idx = line.indexOf(":");
    if (idx === -1) continue;
    const key = line.slice(0, idx).trim();
    const value = line.slice(idx + 1).trim();
    if (key) out[key] = value;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** Render a stored headers object back into "Key: Value" lines for editing. */
export function stringifyHeaders(headers: unknown): string {
  if (!headers || typeof headers !== "object") return "";
  return Object.entries(headers as Record<string, unknown>)
    .map(([key, value]) => `${key}: ${value}`)
    .join("\n");
}
