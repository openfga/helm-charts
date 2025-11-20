{{/*
Expand the name of the chart.
*/}}
{{- define "openfga.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

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
