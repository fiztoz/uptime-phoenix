{{/*
┌──────────────────────────────────────────────────────────────────────────────┐
│ Phoenix Helm helpers                                                         │
└──────────────────────────────────────────────────────────────────────────────┘
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "phoenix.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "phoenix.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "phoenix.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "phoenix.labels" -}}
helm.sh/chart: {{ include "phoenix.chart" . }}
{{ include "phoenix.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: phoenix
{{- end }}

{{/*
Selector labels (used in Deployment, Service, PDB)
*/}}
{{- define "phoenix.selectorLabels" -}}
app.kubernetes.io/name: {{ include "phoenix.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
App image reference. Empty image.tag falls back to Chart.AppVersion so a
chart bump rolls pods onto the matching published image.
*/}}
{{- define "phoenix.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/*
Split-web image reference. Empty web.image.tag falls back to Chart.AppVersion.
*/}}
{{- define "phoenix.webImage" -}}
{{- printf "%s:%s" .Values.web.image.repository (.Values.web.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/*
Read one key out of a looked-up Secret's data map, or an empty string when the
Secret/key is absent (helm template, first install, pre-created Secret without
the key).
*/}}
{{- define "phoenix.secretValueOrEmpty" -}}
{{- $data := .data | default dict }}
{{- if hasKey $data .key }}
{{- index $data .key | b64dec }}
{{- end }}
{{- end }}

{{/*
Database client binaries for the maintenance CronJobs and the dbMigration Job.

The official mariadb:11 image — the chart's own default for cronJobs.*.image and
dbMigration.image — ships mariadb / mariadb-dump / mariadb-admin and NO LONGER
provides the legacy mysql / mysqldump / mysqladmin names, which are also absent
from MariaDB 11 entirely. Resolving at runtime keeps the jobs working on both
the new and the legacy names (older mariadb tags, MySQL-derived images) and
fails loudly when neither exists, instead of running a Job that pretends to
succeed.

POSIX-sh safe (the CronJob shells run /bin/sh = dash on the mariadb image).

Usage: {{ include "phoenix.dbClientResolve" . | nindent 18 }}

Emits the resolved variables PLUS legacy-name shell functions, so a script can
keep calling `mysql` / `mysqldump` / `mysqladmin` and still land on whichever
name the image actually ships. Subshells (`$(mysql ...)`) inherit POSIX shell
functions, so command substitutions are covered too.
*/}}
{{- define "phoenix.dbClientResolve" -}}
_db_bin() {
  for _cand in "$@"; do
    if command -v "$_cand" >/dev/null 2>&1; then
      printf '%s\n' "$_cand"
      return 0
    fi
  done
  echo "ERROR: none of [$*] exists in this image; pin a MariaDB/MySQL client image" >&2
  return 1
}
DB_CLIENT=$(_db_bin mariadb mysql)
DB_DUMP=$(_db_bin mariadb-dump mysqldump)
DB_ADMIN=$(_db_bin mariadb-admin mysqladmin)
mysql() { "$DB_CLIENT" "$@"; }
mysqldump() { "$DB_DUMP" "$@"; }
mysqladmin() { "$DB_ADMIN" "$@"; }
{{- end }}

{{/*
In-release MariaDB credentials.

Why the cache exists: three templates must agree on one generated password —
secret.yaml writes it under two keys, the StatefulSet and the wait-for-mariadb
gate read those keys, and configmap.yaml embeds it as a LITERAL inside DB_DSN.
randAlphaNum returns a new value on every call, so on a first install (where
`lookup` still sees no Secret) the naive version rendered a DSN whose password
was not the one the MariaDB Pod booted with. The symptom is precisely the one
this chart used to hide: every manifest looks healthy, the init gate passes, and
Phoenix dies with "Access denied for user 'phoenix'". Caught by deploying the
chart to a real cluster; `helm template` alone cannot see it.

Helm renders all templates of one release in a single pass against a shared
.Values map, so caching the pair under private keys makes them identical within
one render. It does not leak into `helm get values` (the release record stores
the user-supplied values, not this mutated copy).

Retention across upgrades still comes from `lookup` against the release Secret:
MariaDB builds its user table only on an EMPTY data directory, so a password
that changes between installs would lock Phoenix out of an existing database.
Render-only GitOps controllers have no `lookup` and MUST supply
mariadb.rootPassword from a protected values source (same rule as secret.jwt).

Password charset: the DSN is user:password@tcp(host:port)/db — a credential
containing @, /, ? or = is not escaped and will break parsing, so prefer the
generated alphanumeric value or a password without those characters.

Each key is retained independently: MariaDB initialised its `phoenix` user from
the `mariadb-password` key, so that key (not the root one) is the value the
running datadir actually accepts. Falling back to root unconditionally would
re-write the DSN to a credential the existing database never learned.
*/}}
{{- define "phoenix.mariadbEnsureCredentials" -}}
{{- if not (hasKey .Values "__phoenixMdbRootPassword") }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "phoenix.fullname" .) }}
{{- $data := dict }}
{{- if $existing }}
{{- $data = $existing.data | default dict }}
{{- end }}
{{- $root := .Values.mariadb.rootPassword | default "" }}
{{- if not $root }}
{{- $root = include "phoenix.secretValueOrEmpty" (dict "data" $data "key" "mariadb-root-password") }}
{{- end }}
{{- if not $root }}
{{- $root = randAlphaNum 32 }}
{{- end }}
{{- $user := .Values.mariadb.auth.password | default "" }}
{{- if not $user }}
{{- $user = include "phoenix.secretValueOrEmpty" (dict "data" $data "key" "mariadb-password") }}
{{- end }}
{{- if not $user }}
{{- $user = $root }}
{{- end }}
{{- $_ := set .Values "__phoenixMdbRootPassword" $root }}
{{- $_ := set .Values "__phoenixMdbUserPassword" $user }}
{{- end }}
{{- end }}

{{/*
Root password for the in-release MariaDB.
*/}}
{{- define "phoenix.mariadbRootPassword" -}}
{{- include "phoenix.mariadbEnsureCredentials" . }}
{{- index .Values "__phoenixMdbRootPassword" }}
{{- end }}

{{/*
Password for the application database user (mariadb.auth.username). Falls back
to the root password so a single credential is generated and retained.
*/}}
{{- define "phoenix.mariadbUserPassword" -}}
{{- include "phoenix.mariadbEnsureCredentials" . }}
{{- index .Values "__phoenixMdbUserPassword" }}
{{- end }}

{{/*
Database password the Phoenix DSN authenticates with — the application user's
password for the in-release MariaDB, mariadbExternal.password otherwise.
*/}}
{{- define "phoenix.dbPassword" -}}
{{- if .Values.mariadb.enabled }}
{{- include "phoenix.mariadbUserPassword" . }}
{{- else }}
{{- .Values.mariadbExternal.password }}
{{- end }}
{{- end }}

{{/*
JWT secret — auto-generated if not provided.
Stored in the main Secret under jwt-secret key.
*/}}
{{- define "phoenix.jwtSecret" -}}
{{- if .Values.secret.jwt }}
{{- .Values.secret.jwt }}
{{- else }}
{{- $secretName := include "phoenix.fullname" . }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName }}
{{- if $existing }}
{{- if index $existing.data "jwt-secret" }}
{{- index $existing.data "jwt-secret" | b64dec }}
{{- else }}
{{- randAlphaNum 48 }}
{{- end }}
{{- else }}
{{- randAlphaNum 48 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Secret and key that provide the JWT signing key. A pre-created Secret is the
stable option for render-only GitOps controllers, where lookup is unavailable.
*/}}
{{- define "phoenix.jwtSecretName" -}}
{{- default (include "phoenix.fullname" .) .Values.secret.existingSecret -}}
{{- end }}

{{- define "phoenix.jwtSecretKey" -}}
{{- default "jwt-secret" .Values.secret.existingSecretKey -}}
{{- end }}

{{/*
DB engine (sqlite or mariadb)
*/}}
{{- define "phoenix.dbEngine" -}}
{{- .Values.database.engine | default "sqlite" }}
{{- end }}

{{/*
DB DSN constructed based on engine
For sqlite default: file:/data/phoenix.db
For mariadb: constructed from sub values
*/}}
{{- define "phoenix.dbDSN" -}}
{{- $engine := include "phoenix.dbEngine" . }}
{{- if eq $engine "sqlite" }}{{- if .Values.database.dsn }}{{- .Values.database.dsn }}{{- else }}file:/data/phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000){{- end }}{{- else if eq $engine "mariadb" }}
{{- if .Values.mariadb.enabled }}
{{- printf "%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC" .Values.mariadb.auth.username (include "phoenix.dbPassword" .) (include "phoenix.mariadbHost" .) (include "phoenix.mariadb.port" . | int) .Values.mariadb.auth.database }}
{{- else if .Values.mariadbExternal.host }}
{{- printf "%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC" .Values.mariadbExternal.username (include "phoenix.dbPassword" .) .Values.mariadbExternal.host (int .Values.mariadbExternal.port) .Values.mariadbExternal.database }}
{{- else }}
{{- fail "mariadb.enabled or mariadbExternal.host required when database.engine=mariadb" }}
{{- end }}
{{- else }}
{{- fail "Unsupported database.engine (use sqlite or mariadb)" }}
{{- end }}
{{- end }}

{{/*
MariaDB hostname for in-cluster or external connections.
*/}}
{{- define "phoenix.mariadbHost" -}}
{{- if .Values.mariadb.enabled }}
{{- include "phoenix.mariadb.fullname" . }}
{{- else }}
{{- .Values.mariadbExternal.host }}
{{- end }}
{{- end }}

{{/*
In-release MariaDB resource names, labels and port.

phoenix.mariadb.selectorLabels deliberately does NOT reuse
phoenix.selectorLabels: the all-in-one Service selects on name+instance alone,
so a MariaDB Pod carrying those labels would receive Phoenix HTTP traffic, and
the PDB would start counting the database Pod.
*/}}
{{- define "phoenix.mariadb.name" -}}
{{- printf "%s-mariadb" (include "phoenix.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "phoenix.mariadb.fullname" -}}
{{- if .Values.mariadb.fullnameOverride }}
{{- .Values.mariadb.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-mariadb" (include "phoenix.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "phoenix.mariadb.port" -}}
{{- int (default 3306 .Values.mariadb.service.port) }}
{{- end }}

{{- define "phoenix.mariadb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "phoenix.mariadb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: mariadb
{{- end }}

{{- define "phoenix.mariadb.labels" -}}
helm.sh/chart: {{ include "phoenix.chart" . }}
{{ include "phoenix.mariadb.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: phoenix
{{- end }}

{{/*
MariaDB image reference. Empty tag falls back to the chart's default so the
server version tracks what the migrations were verified against.
*/}}
{{- define "phoenix.mariadb.image" -}}
{{- printf "%s:%s" .Values.mariadb.image.repository (default "11" .Values.mariadb.image.tag) }}
{{- end }}

{{/*
Server flags for the in-release MariaDB. Passed as container args, which the
official mariadb entrypoint forwards to BOTH mariadb-install-db (so the
created schema gets this charset/collation) and mariadbd.

character-set-server/collation-server matter because the MariaDB migrations do
not emit per-table CHARSETS — the server default is what every table gets, and
utf8mb4_unicode_ci is also what the dbMigration Job assumes.
default-time-zone=+00:00 keeps the server wall-clock UTC, matching rule 6 of
AGENTS.md (heartbeats.time is a second-precision TIMESTAMP and Phoenix stores
UTC wall-clock).
*/}}
{{- define "phoenix.mariadb.serverArgs" -}}
- --character-set-server={{ .Values.mariadb.config.characterSet }}
- --collation-server={{ .Values.mariadb.config.collation }}
- --default-time-zone=+00:00
- --skip-name-resolve
- --max-connections={{ int .Values.mariadb.config.maxConnections }}
{{- if .Values.mariadb.config.innodbBufferPoolSize }}
- --innodb-buffer-pool-size={{ .Values.mariadb.config.innodbBufferPoolSize }}
{{- end }}
{{- range .Values.mariadb.config.extraArgs }}
- {{ . }}
{{- end }}
{{- end }}

{{/*
Refuse ambiguous database configurations instead of rendering a healthy-looking
release that talks to the wrong server.
*/}}
{{- define "phoenix.validateMariaDB" -}}
{{- if .Values.mariadb.enabled }}
{{- if ne (include "phoenix.dbEngine" .) "mariadb" }}
{{- fail "mariadb.enabled=true requires database.engine=mariadb (otherwise Phoenix would still boot on SQLite while an unused MariaDB is deployed)" }}
{{- end }}
{{- if .Values.mariadbExternal.host }}
{{- fail "mariadb.enabled=true and mariadbExternal.host are mutually exclusive; set mariadb.enabled=false to use the external server" }}
{{- end }}
{{- if .Values.mariadb.persistence.enabled }}
{{- if not .Values.mariadb.persistence.size }}
{{- fail "mariadb.persistence.size is required when mariadb.persistence.enabled=true" }}
{{- end }}
{{- if not (hasKey (dict "keep" true "delete" true) (default "keep" .Values.mariadb.persistence.resourcePolicy)) }}
{{- fail "mariadb.persistence.resourcePolicy must be exactly \"keep\" or \"delete\" (a typo would silently drop the protection that keeps helm uninstall from deleting the database)" }}
{{- end }}
{{- else if not .Values.mariadb.allowEphemeralData }}
{{- fail "mariadb.persistence.enabled=false discards all monitoring history on every Pod restart; set mariadb.allowEphemeralData=true to opt in (dev/testing only)" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Valkey subchart resource name. This mirrors the official chart's fullname
helper so parent Deployments can reference its primary Service.
*/}}
{{- define "phoenix.valkeyFullname" -}}
{{- if .Values.valkey.fullnameOverride }}
{{- .Values.valkey.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default "valkey" .Values.valkey.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Secret containing the Valkey default-user password.
*/}}
{{- define "phoenix.valkeyAuthSecretName" -}}
{{- tpl .Values.valkey.auth.usersExistingSecret . | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Key containing the Valkey default-user password.
*/}}
{{- define "phoenix.valkeyPasswordKey" -}}
{{- $defaultUser := index .Values.valkey.auth.aclUsers "default" | default dict }}
{{- default "default" $defaultUser.passwordKey }}
{{- end }}

{{/*
Generate the managed Valkey password once, then retain it across upgrades.
*/}}
{{- define "phoenix.valkeyPassword" -}}
{{- if .Values.valkey.auth.password }}
{{- .Values.valkey.auth.password }}
{{- else }}
{{- $secretName := include "phoenix.valkeyAuthSecretName" . }}
{{- $passwordKey := include "phoenix.valkeyPasswordKey" . }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName }}
{{- if and $existing (index $existing.data $passwordKey) }}
{{- index $existing.data $passwordKey | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
EventBus port used by Phoenix and its egress NetworkPolicy.
*/}}
{{- define "phoenix.eventBusPort" -}}
{{- if .Values.valkey.enabled }}
{{- int (default 6379 .Values.valkey.service.port) }}
{{- else }}
{{- int (default 6379 .Values.redis.port) }}
{{- end }}
{{- end }}

{{/*
REDIS_URL wiring shared by all, API, and worker Deployments.
*/}}
{{- define "phoenix.eventBusEnv" -}}
{{- if and .Values.valkey.enabled .Values.redis.enabled }}
{{- fail "valkey.enabled and redis.enabled are mutually exclusive; choose in-release Valkey or external Redis" }}
{{- end }}
{{- if .Values.valkey.enabled }}
{{- if .Values.valkey.tls.enabled }}
{{- fail "valkey.tls.enabled is not supported by automatic Phoenix wiring because client certificates are not configured" }}
{{- end }}
{{- if and .Values.valkey.auth.enabled (not (hasKey .Values.valkey.auth.aclUsers "default")) }}
{{- fail "valkey.auth.aclUsers.default is required by automatic Phoenix wiring" }}
{{- end }}
{{- if and .Values.valkey.auth.enabled (not .Values.valkey.auth.usersExistingSecret) }}
{{- fail "valkey.auth.usersExistingSecret is required by automatic Phoenix wiring" }}
{{- end }}
{{- $host := printf "%s.%s.svc.%s" (include "phoenix.valkeyFullname" .) .Release.Namespace (default "cluster.local" .Values.valkey.clusterDomain) }}
{{- $port := include "phoenix.eventBusPort" . }}
{{- if .Values.valkey.auth.enabled }}
- name: VALKEY_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "phoenix.valkeyAuthSecretName" . }}
      key: {{ include "phoenix.valkeyPasswordKey" . }}
- name: REDIS_URL
  value: {{ printf "redis://default:$(VALKEY_PASSWORD)@%s:%s/0" $host $port | quote }}
{{- else }}
- name: REDIS_URL
  value: {{ printf "redis://%s:%s/0" $host $port | quote }}
{{- end }}
{{- else if .Values.redis.enabled }}
{{- if and (not .Values.redis.existingSecret) (not .Values.redis.host) }}
{{- fail "redis.host is required when redis.enabled=true and redis.existingSecret is empty" }}
{{- end }}
- name: REDIS_URL
  valueFrom:
    secretKeyRef:
      {{- if .Values.redis.existingSecret }}
      name: {{ .Values.redis.existingSecret }}
      key: {{ .Values.redis.existingSecretKey | default "redis-url" }}
      {{- else }}
      name: {{ include "phoenix.fullname" . }}
      key: redis-url
      {{- end }}
{{- end }}
{{- end }}

{{/*
Init containers that block Phoenix until the in-release dependencies it cannot
retry against are ready.

Valkey is gated on TCP only: REDIS_URL is selected once at process start and a
missed first ping permanently falls back to the in-memory bus, so split
API/worker must not race Valkey.

MariaDB is gated on an authenticated `SELECT 1` against the target database,
not on TCP. `mariadb-admin ping` exits 0 even on access-denied, so it cannot
catch the one failure mode that matters here: the Secret holds a password the
existing data directory never learned (mariadb bootstraps users only on an
empty datadir). A bounded loop with a loud failure is used instead of an
unbounded `until`, so a wrong credential surfaces as a clear init-container
error rather than an eternal CrashLoopBackOff.
*/}}
{{- define "phoenix.waitForDependenciesInitContainers" -}}
{{- if .Values.mariadb.enabled }}
- name: wait-for-mariadb
  image: {{ include "phoenix.mariadb.image" . | quote }}
  command:
    - /bin/bash
    - -c
    - |
      set -eu
      {{- include "phoenix.dbClientResolve" . | nindent 6 }}
      attempts={{ int (default 60 .Values.mariadb.waitFor.attempts) }}
      for i in $(seq 1 "$attempts"); do
        if "$DB_CLIENT" -h "$PHOENIX_DB_HOST" -P "$PHOENIX_DB_PORT" -u "$PHOENIX_DB_USER" "$PHOENIX_DB_NAME" -e 'SELECT 1' >/dev/null 2>&1; then
          echo "mariadb accepts authenticated connections to $PHOENIX_DB_NAME."
          exit 0
        fi
        echo "($i/$attempts) waiting for mariadb at $PHOENIX_DB_HOST:$PHOENIX_DB_PORT..."
        sleep 5
      done
      echo "ERROR: $PHOENIX_DB_HOST:$PHOENIX_DB_PORT never accepted an authenticated connection as $PHOENIX_DB_USER."
      echo "       Check the mariadb Pod logs, and note that a password changed in the Secret is NOT"
      echo "       applied to an existing data directory (mariadb bootstraps users only when it is empty)."
      exit 1
  env:
    - name: PHOENIX_DB_HOST
      value: {{ include "phoenix.mariadbHost" . | quote }}
    - name: PHOENIX_DB_PORT
      value: {{ include "phoenix.mariadb.port" . | quote }}
    - name: PHOENIX_DB_USER
      value: {{ .Values.mariadb.auth.username | quote }}
    - name: PHOENIX_DB_NAME
      value: {{ .Values.mariadb.auth.database | quote }}
    # MYSQL_PWD keeps the credential out of the container argv (visible in ps).
    - name: MYSQL_PWD
      valueFrom:
        secretKeyRef:
          name: {{ include "phoenix.fullname" . }}
          key: mariadb-password
  securityContext:
    {{- toYaml .Values.containerSecurityContext | nindent 4 }}
{{- end }}
{{- if .Values.valkey.enabled }}
- name: wait-for-valkey
  image: busybox:1.36
  command:
    - sh
    - -c
    - until nc -z {{ include "phoenix.valkeyFullname" . }} {{ include "phoenix.eventBusPort" . }}; do echo "waiting for valkey..."; sleep 2; done
  securityContext:
    {{- toYaml .Values.containerSecurityContext | nindent 4 }}
{{- end }}
{{- end }}

{{/*
Join a Helm value that may be a YAML list or a comma-separated string into the
CSV form Phoenix env vars expect (OIDC_ADMIN_GROUPS, OIDC_SCOPES, …).

  oidc.adminGroups: ["a", "b"]     →  a,b
  oidc.adminGroups: "a, b"         →  a, b
  oidc.adminGroups: []  /  ""      →  (empty)

Empty items are dropped. A comma-separated string is passed through so existing
values files keep working.
*/}}
{{- define "phoenix.csv" -}}
{{- if kindIs "slice" . -}}
{{- $parts := list -}}
{{- range . -}}
{{- $s := trim (toString .) -}}
{{- if $s -}}
{{- $parts = append $parts $s -}}
{{- end -}}
{{- end -}}
{{- join "," $parts -}}
{{- else if not (empty .) -}}
{{- trim (toString .) -}}
{{- end -}}
{{- end }}

{{/*
Shared Phoenix application env (security, observability, rate limits).
*/}}
{{- define "phoenix.envAppConfig" -}}
- name: DB_MAX_OPEN_CONNS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: db-max-open-conns
- name: DB_MAX_IDLE_CONNS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: db-max-idle-conns
- name: DB_CONN_MAX_IDLE_SECONDS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: db-conn-max-idle-seconds
- name: DB_CONN_MAX_LIFETIME_SECONDS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: db-conn-max-lifetime-seconds
- name: LOG_LEVEL
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: log-level
- name: PRODUCTION
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: production
- name: CORS_ALLOW_ORIGINS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: cors-allow-origins
- name: RATE_LIMIT_RPS
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: rate-limit-rps
- name: RATE_LIMIT_BURST
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: rate-limit-burst
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: otel-exporter-endpoint
- name: OTEL_SERVICE_NAME
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: otel-service-name
- name: PUBLIC_URL
  valueFrom:
    configMapKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: public-url
- name: PHOENIX_EXTENSIONS
  valueFrom:
    secretKeyRef:
      name: {{ include "phoenix.fullname" . }}
      key: extensions-json
{{- if .Values.oidc.enabled }}
- name: OIDC_ISSUER
  value: {{ .Values.oidc.issuer | quote }}
- name: OIDC_CLIENT_ID
  value: {{ .Values.oidc.clientId | quote }}
- name: OIDC_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      {{- if .Values.oidc.existingSecret }}
      name: {{ .Values.oidc.existingSecret }}
      key: {{ .Values.oidc.secretKey | default "oidc-client-secret" }}
      {{- else }}
      name: {{ include "phoenix.fullname" . }}
      key: oidc-client-secret
      {{- end }}
- name: OIDC_REDIRECT_URL
  value: {{ .Values.oidc.redirectUrl | quote }}
- name: OIDC_SCOPES
  value: {{ include "phoenix.csv" .Values.oidc.scopes | quote }}
- name: OIDC_GROUPS_CLAIM
  value: {{ .Values.oidc.groupsClaim | quote }}
- name: OIDC_JIT_ENABLED
  value: {{ .Values.oidc.jitEnabled | quote }}
- name: OIDC_LINK_BY_EMAIL
  value: {{ .Values.oidc.linkByEmail | quote }}
- name: OIDC_ALLOWED_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.allowedGroups | quote }}
- name: OIDC_ADMIN_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.adminGroups | quote }}
- name: OIDC_CAP_NOTIFICATIONS_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capNotificationsGroups | quote }}
- name: OIDC_CAP_MAINTENANCE_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capMaintenanceGroups | quote }}
- name: OIDC_CAP_CREATE_MONITORS_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capCreateMonitorsGroups | quote }}
- name: OIDC_CAP_CREATE_GROUPS_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capCreateGroupsGroups | quote }}
- name: OIDC_CAP_EDIT_GROUP_METADATA_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capEditGroupMetadataGroups | quote }}
- name: OIDC_CAP_VIEW_EXTENSIONS_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capViewExtensionsGroups | quote }}
- name: OIDC_CAP_VIEW_ALL_MONITORS_GROUPS
  value: {{ include "phoenix.csv" .Values.oidc.capViewAllMonitorsGroups | quote }}
- name: OIDC_GRANT_MAP
  value: {{ include "phoenix.csv" .Values.oidc.grantMap | quote }}
{{- end }}
{{- end }}

{{/*
Stable fingerprint of values that become Secrets consumed as env by the
Phoenix API / worker (and the all-in-one Deployment).

Do not hash rendered secret.yaml / secret-valkey.yaml: those call lookup
plus randAlphaNum, which is non-deterministic when the renderer has no
cluster access (Argo CD `helm template`).
*/}}
{{- define "phoenix.secretFingerprint" -}}
redis:{{ .Values.redis | toYaml }}
oidc:{{ .Values.oidc | toYaml }}
extensions:{{ .Values.extensions | toYaml }}
bootstrap:{{ .Values.bootstrap | toYaml }}
mariadb.enabled={{ .Values.mariadb.enabled }}
mariadb.rootPassword={{ .Values.mariadb.rootPassword }}
mariadb.auth:{{ .Values.mariadb.auth | toYaml }}
mariadb.service.port={{ .Values.mariadb.service.port }}
mariadb.config:{{ .Values.mariadb.config | toYaml }}
mariadbExternal:{{ .Values.mariadbExternal | toYaml }}
prometheus.podMonitor.enabled={{ .Values.prometheus.podMonitor.enabled }}
prometheus.apiKey={{ .Values.prometheus.apiKey }}
jwt.existingSecret={{ .Values.secret.existingSecret }}
jwt.existingSecretKey={{ .Values.secret.existingSecretKey }}
jwt.secret={{ .Values.secret.jwt }}
{{- if .Values.valkey.enabled }}
valkey.auth.enabled={{ .Values.valkey.auth.enabled }}
valkey.auth.password={{ .Values.valkey.auth.password }}
valkey.auth.managedSecret={{ .Values.valkey.auth.managedSecret }}
valkey.auth.usersExistingSecret={{ .Values.valkey.auth.usersExistingSecret }}
valkey.service.port={{ .Values.valkey.service.port }}
{{- end }}
{{- end }}

{{/*
Pod-template annotations that change when chart-managed ConfigMaps or
Secrets change. Kubernetes then rolls the Deployment so new pods pick up
the new env. This is what makes an Argo CD sync of config restart API and
worker; updating a ConfigMap/Secret alone does not recreate pods.
*/}}
{{- define "phoenix.configReloadAnnotations" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
checksum/secret: {{ include "phoenix.secretFingerprint" . | sha256sum }}
{{- end }}

{{/*
Deployment name — single pod vs multi mode
*/}}
{{- define "phoenix.deploymentName" -}}
{{- if eq .Values.scaling.mode "single" }}
{{- include "phoenix.fullname" . }}
{{- else }}
{{- printf "%s-api" (include "phoenix.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Cloudflare Tunnel — Secret name holding the connector token.
Uses cloudflareTunnel.existingSecret when set, otherwise the chart-managed Secret.
*/}}
{{- define "phoenix.cloudflared.secretName" -}}
{{- if .Values.cloudflareTunnel.existingSecret }}
{{- .Values.cloudflareTunnel.existingSecret }}
{{- else }}
{{- printf "%s-cloudflared" (include "phoenix.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Reserved Ingress/SPA prefixes that an extension path must not equal or nest under
(element-wise Prefix match: /api blocks /api/foo, not /apifoo).
*/}}
{{- define "phoenix.reservedExtensionPaths" -}}
/api,/ws,/dashboard,/insights,/monitors,/alerts,/notifications,/escalation-policies,/maintenance,/status-pages,/incidents,/backup,/settings,/login,/docs,/extensions
{{- end }}

{{/*
Fail the render when extensions[] is incomplete, collides, or is not DNS-1123.
*/}}
{{- define "phoenix.validateExtensions" -}}
{{- $root := . -}}
{{- $ids := dict -}}
{{- $paths := dict -}}
{{- $reserved := splitList "," (include "phoenix.reservedExtensionPaths" .) -}}
{{- range $i, $ext := (.Values.extensions | default list) }}
{{- if empty $ext.id }}
{{- fail (printf "extensions[%d].id is required" $i) }}
{{- end }}
{{- if empty $ext.title }}
{{- fail (printf "extensions[%d].title is required" $i) }}
{{- end }}
{{- if empty $ext.path }}
{{- fail (printf "extensions[%d].path is required" $i) }}
{{- end }}
{{- if empty $ext.image }}
{{- fail (printf "extensions[%d].image is required" $i) }}
{{- end }}
{{- $id := $ext.id | toString -}}
{{- $path := $ext.path | toString -}}
{{- if or (gt (len $id) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $id)) }}
{{- fail (printf "extensions[%d].id %q is not a valid DNS-1123 label" $i $id) }}
{{- end }}
{{- if gt (len (printf "extension-%s" $id)) 63 }}
{{- fail (printf "extensions[%d].id %q is too long for label app.kubernetes.io/component=extension-%s" $i $id $id) }}
{{- end }}
{{- $resName := printf "%s-ext-%s" (include "phoenix.fullname" $root) $id -}}
{{- if gt (len $resName) 63 }}
{{- fail (printf "extensions[%d] resource name %q exceeds 63 characters; shorten id or fullname" $i $resName) }}
{{- end }}
{{- if or (eq $path "/") (not (hasPrefix "/" $path)) }}
{{- fail (printf "extensions[%d].path %q must start with / and must not be empty or /" $i $path) }}
{{- end }}
{{- if not (empty $ext.icon) }}
{{- $icon := $ext.icon | toString | trim -}}
{{- if or (not (hasPrefix "/" $icon)) (hasPrefix "//" $icon) (contains ":" $icon) (contains ".." $icon) }}
{{- fail (printf "extensions[%d].icon %q must be a same-origin path (start with /, no scheme)" $i $icon) }}
{{- end }}
{{- end }}
{{- range $reserved }}
{{- if or (eq $path .) (hasPrefix (printf "%s/" .) $path) (hasPrefix (printf "%s/" $path) .) }}
{{- fail (printf "extensions[%d].path %q conflicts with reserved path %s" $i $path .) }}
{{- end }}
{{- end }}
{{- if hasKey $ids $id }}
{{- fail (printf "duplicate extensions id %q" $id) }}
{{- end }}
{{- $_ := set $ids $id true -}}
{{- if hasKey $paths $path }}
{{- fail (printf "duplicate extensions path %q" $path) }}
{{- end }}
{{- $_ := set $paths $path true -}}
{{- end }}
{{- end }}

{{/*
Public catalogue JSON: [{id,title,path,icon(,uiToken)}, …] only. Never image
or DB credentials. uiToken (optional) is the extension's UI_TOKEN launch
credential; Phoenix releases it only through the gated /frame redirect and
never echoes it on GET /api/extensions. The whole catalogue lives in the
managed Secret because of it. icon defaults to {path}/icon.svg — the plugin
image must serve that file.
Always validates extensions[] first so a bad values file fails the render.
*/}}
{{- define "phoenix.extensionsCatalog" -}}
{{- include "phoenix.validateExtensions" . -}}
{{- $catalog := list -}}
{{- range .Values.extensions | default list }}
{{- $path := .path | toString -}}
{{- $icon := .icon | default "" | toString | trim -}}
{{- if empty $icon -}}
{{- $icon = printf "%s/icon.svg" (trimSuffix "/" $path) -}}
{{- end }}
{{- $entry := dict "id" (.id | toString) "title" (.title | toString) "path" $path "icon" $icon -}}
{{- with .uiToken | default "" | toString }}
{{- $_ := set $entry "uiToken" . -}}
{{- end }}
{{- $catalog = append $catalog $entry -}}
{{- end }}
{{- $catalog | toJson -}}
{{- end }}

{{/*
Workload name / Service for one extension: <fullname>-ext-<id>
Expects dict "root" $ "id" <id>.
*/}}
{{- define "phoenix.extensionName" -}}
{{- printf "%s-ext-%s" (include "phoenix.fullname" .root) .id -}}
{{- end }}

{{/*
Selector labels for one extension. Must not reuse phoenix.selectorLabels —
the all-in-one Service selects name+instance only and would steal traffic.
Expects dict "root" $ "id" <id>.
*/}}
{{- define "phoenix.extensionSelectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-ext" (include "phoenix.name" .root) }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ printf "extension-%s" .id }}
{{- end }}

{{/*
Common labels for one extension workload.
Expects dict "root" $ "id" <id>.
*/}}
{{- define "phoenix.extensionLabels" -}}
helm.sh/chart: {{ include "phoenix.chart" .root }}
{{ include "phoenix.extensionSelectorLabels" . }}
{{- if .root.Chart.AppVersion }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: phoenix
{{- end }}
