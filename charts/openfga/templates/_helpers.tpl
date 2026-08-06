{{/*
Expand the name of the chart.
*/}}
{{- define "openfga.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Health probe handler.
Newer OpenFGA images no longer bundle the grpc_health_probe binary, so probes
rely on Kubernetes-native handlers where possible and on the in-binary
`openfga healthcheck` command for the case native handlers cannot cover.

- When the HTTP server is enabled: httpGet /healthz (scheme follows
  http.tls.enabled). /healthz is a grpc-gateway proxy to the gRPC health
  service, so it faithfully reflects overall health.
- When HTTP is disabled and gRPC is plaintext: the native grpc: handler.
- When HTTP is disabled and gRPC TLS is enabled: an exec probe running
  `openfga healthcheck`. The Kubernetes-native grpc: handler is plaintext-only
  and would fail the TLS handshake, so it cannot be used here. The healthcheck
  command reads the same OPENFGA_GRPC_* env vars the container already sets
  (addr, tls.enabled, tls.cert), so it probes the gRPC health service over TLS
  and verifies the server against the configured certificate with no extra
  configuration.
*/}}
{{- define "openfga.probeHandler" -}}
{{- if .Values.http.enabled -}}
httpGet:
  path: /healthz
  port: {{ (split ":" .Values.http.addr)._1 }}
  scheme: {{ if .Values.http.tls.enabled }}HTTPS{{ else }}HTTP{{ end }}
{{- else if .Values.grpc.tls.enabled -}}
exec:
  command:
    - /openfga
    - healthcheck
    - --target
    - grpc
{{- else -}}
grpc:
  port: {{ (split ":" .Values.grpc.addr)._1 }}
{{- end -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "openfga.fullname" -}}
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
Expand the namespace of the release.
Allows overriding it for multi-namespace deployments in combined charts.
*/}}
{{- define "openfga.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "openfga.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "openfga.labels" -}}
helm.sh/chart: {{ include "openfga.chart" . }}
{{ include "openfga.selectorLabels" . }}
{{- with .Values.commonLabels }}
{{ . | toYaml }}
{{- end }}
app.kubernetes.io/component: authorization-controller
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: openfga
{{- end }}

{{/*
Selector labels
*/}}
{{- define "openfga.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openfga.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "openfga.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "openfga.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Return true if a secret object should be created
*/}}
{{- define "openfga.createSecret" -}}
{{- if not (or .Values.global.postgresql.auth.existingSecret .Values.auth.existingSecret) -}}
    {{- true -}}
{{- end -}}
{{- end -}}

{{- define "openfga.datastore.secretName" -}}
{{ include "openfga.fullname" . }}-datastore-secret
{{- end -}}

{{- define "openfga.datastore.secondary.secretName" -}}
{{ include "openfga.fullname" . }}-secondary-datastore-secret
{{- end -}}

{{- define "openfga.datastore.secondary.envConfig" -}}
{{- if .Values.datastore.secondary.engine -}}
- name: OPENFGA_DATASTORE_SECONDARY_ENGINE
  value: "{{ .Values.datastore.secondary.engine }}"
{{- end -}}

{{- if .Values.datastore.secondary.uriSecret }}
- name: OPENFGA_DATASTORE_SECONDARY_URI
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.secondary.uriSecret }}"
      key: uri
{{- else if and (.Values.datastore.secondary.existingSecret) (.Values.datastore.secondary.secretKeys.uriKey) }}
- name: OPENFGA_DATASTORE_SECONDARY_URI
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.secondary.existingSecret }}"
      key: "{{ .Values.datastore.secondary.secretKeys.uriKey }}"
{{- else if .Values.datastore.secondary.uri }}
- name: OPENFGA_DATASTORE_SECONDARY_URI
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secondary.secretName" . | quote }}
      key: "uri"
{{- end -}}

{{- if and (.Values.datastore.secondary.existingSecret) (.Values.datastore.secondary.secretKeys.usernameKey) }}
- name: OPENFGA_DATASTORE_SECONDARY_USERNAME
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.secondary.existingSecret }}"
      key: "{{ .Values.datastore.secondary.secretKeys.usernameKey }}"
{{- else if .Values.datastore.secondary.username }}
- name: OPENFGA_DATASTORE_SECONDARY_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secondary.secretName" . | quote }}
      key: "username"
{{- end -}}

{{- if and (.Values.datastore.secondary.existingSecret) (.Values.datastore.secondary.secretKeys.passwordKey) }}
- name: OPENFGA_DATASTORE_SECONDARY_PASSWORD
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.secondary.existingSecret }}"
      key: "{{ .Values.datastore.secondary.secretKeys.passwordKey }}"
{{- else if .Values.datastore.secondary.password }}
- name: OPENFGA_DATASTORE_SECONDARY_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secondary.secretName" . | quote }}
      key: "password"
{{- end -}}
{{- end -}}

{{- define "openfga.datastore.envConfig" -}}
{{- if .Values.datastore.engine -}}
- name: OPENFGA_DATASTORE_ENGINE
  value: "{{ .Values.datastore.engine }}"
{{- end -}}

{{- if .Values.datastore.uriSecret }}
- name: OPENFGA_DATASTORE_URI
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.uriSecret }}"
      key: uri
{{- else if and (.Values.datastore.existingSecret) (.Values.datastore.secretKeys.uriKey) }}
- name: OPENFGA_DATASTORE_URI
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.existingSecret }}"
      key: "{{ .Values.datastore.secretKeys.uriKey }}"
{{- else if .Values.datastore.uri }}
- name: OPENFGA_DATASTORE_URI
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secretName" . | quote }}
      key: "uri"
{{- end -}}

{{- if and (.Values.datastore.existingSecret) (.Values.datastore.secretKeys.usernameKey) }}
- name: OPENFGA_DATASTORE_USERNAME
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.existingSecret }}"
      key: "{{ .Values.datastore.secretKeys.usernameKey }}"
{{- else if .Values.datastore.username }}
- name: OPENFGA_DATASTORE_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secretName" . | quote }}
      key: "username"
{{- end -}}

{{- if and (.Values.datastore.existingSecret) (.Values.datastore.secretKeys.passwordKey) }}
- name: OPENFGA_DATASTORE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: "{{ .Values.datastore.existingSecret }}"
      key: "{{ .Values.datastore.secretKeys.passwordKey }}"
{{- else if .Values.datastore.password }}
- name: OPENFGA_DATASTORE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "openfga.datastore.secretName" . | quote }}
      key: "password"
{{- end -}}
{{- end -}}
