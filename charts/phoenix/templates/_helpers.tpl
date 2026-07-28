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
Database password — from values or auto-generated (for mariadb).
*/}}
{{- define "phoenix.dbPassword" -}}
{{- if .Values.mariadb.rootPassword }}
{{- .Values.mariadb.rootPassword }}
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
