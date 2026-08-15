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
Database password — from values or auto-generated (for mariadb).
*/}}
{{- define "phoenix.dbPassword" -}}
{{- if and (not .Values.mariadb.enabled) .Values.mariadbExternal.password }}
{{- .Values.mariadbExternal.password }}
{{- else if .Values.mariadb.rootPassword }}
{{- .Values.mariadb.rootPassword }}
{{- else if not .Values.mariadb.enabled }}
{{- .Values.mariadbExternal.password }}
{{- else }}
{{- $secretName := printf "%s-gen" (include "phoenix.fullname" .) }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName }}
{{- if $existing }}
{{- index $existing.data "mariadb-root-password" | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
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
{{- printf "phoenix:%s@tcp(%s-mariadb:3306)/phoenix?parseTime=true&loc=UTC" (include "phoenix.dbPassword" .) (include "phoenix.fullname" .) }}
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
{{- printf "%s-mariadb" (include "phoenix.fullname" .) }}
{{- else }}
{{- .Values.mariadbExternal.host }}
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
Init containers that block Phoenix until in-release Valkey is accepting TCP.
REDIS_URL is selected once at process start; a missed first ping permanently
falls back to the in-memory bus, so split API/worker must not race Valkey.
*/}}
{{- define "phoenix.waitForEventBusInitContainers" -}}
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
Shared Phoenix application env (security, observability, rate limits).
*/}}
{{- define "phoenix.envAppConfig" -}}
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
  value: {{ .Values.oidc.scopes | quote }}
- name: OIDC_GROUPS_CLAIM
  value: {{ .Values.oidc.groupsClaim | quote }}
- name: OIDC_JIT_ENABLED
  value: {{ .Values.oidc.jitEnabled | quote }}
- name: OIDC_LINK_BY_EMAIL
  value: {{ .Values.oidc.linkByEmail | quote }}
- name: OIDC_ALLOWED_GROUPS
  value: {{ .Values.oidc.allowedGroups | quote }}
- name: OIDC_ADMIN_GROUPS
  value: {{ .Values.oidc.adminGroups | quote }}
- name: OIDC_CAP_NOTIFICATIONS_GROUPS
  value: {{ .Values.oidc.capNotificationsGroups | quote }}
- name: OIDC_CAP_MAINTENANCE_GROUPS
  value: {{ .Values.oidc.capMaintenanceGroups | quote }}
- name: OIDC_CAP_CREATE_MONITORS_GROUPS
  value: {{ .Values.oidc.capCreateMonitorsGroups | quote }}
- name: OIDC_CAP_CREATE_GROUPS_GROUPS
  value: {{ .Values.oidc.capCreateGroupsGroups | quote }}
- name: OIDC_GRANT_MAP
  value: {{ .Values.oidc.grantMap | quote }}
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
bootstrap:{{ .Values.bootstrap | toYaml }}
mariadb.enabled={{ .Values.mariadb.enabled }}
mariadb.rootPassword={{ .Values.mariadb.rootPassword }}
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
