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
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
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
Create the name of the service account to use for the migration job
*/}}
{{- define "openfga.migrationServiceAccountName" -}}
{{- if .Values.migrate.serviceAccount.name }}
{{- default "default" .Values.serviceAccount.name }}
{{- else if .Values.migrate.serviceAccount.create }}
{{- default (printf "%s-%s" (include "openfga.fullname" .) "migrate") .Values.migrate.serviceAccount.name }}
{{- else }}
{{- include "openfga.serviceAccountName" . }}
{{- end }}
{{- end }}

{{/*
Return true if migration job is enabled
*/}}
{{- define "openfga.haveMigration" -}}
{{- if and (has .Values.datastore.engine (list "postgres" "mysql")) .Values.datastore.applyMigrations }}
  {{- true -}}
{{- end -}}
{{- end -}}

{{/*
Return true if a secret object should be created
*/}}
{{- define "openfga.createSecret" -}}
{{- if not (or .Values.global.postgresql.auth.existingSecret .Values.auth.existingSecret) -}}
    {{- true -}}
{{- end -}}
{{- end -}}