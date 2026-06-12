{{/*
Chart name
*/}}
{{- define "tsm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name
*/}}
{{- define "tsm.fullname" -}}
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
Chart label
*/}}
{{- define "tsm.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "tsm.labels" -}}
helm.sh/chart: {{ include "tsm.chart" . }}
{{ include "tsm.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Values.backend.image.tag | default .Chart.AppVersion | quote }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "tsm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tsm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "tsm.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "tsm.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Backend image
*/}}
{{- define "tsm.backendImage" -}}
{{- printf "%s:%s" .Values.backend.image.repository (.Values.backend.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/*
Frontend image
*/}}
{{- define "tsm.frontendImage" -}}
{{- printf "%s:%s" .Values.frontend.image.repository (.Values.frontend.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/*
Secret name for app credentials (JWT secret, encryption key, provider secrets)
*/}}
{{- define "tsm.secretName" -}}
{{- if .Values.security.existingSecret }}
{{- .Values.security.existingSecret }}
{{- else }}
{{- include "tsm.fullname" . }}
{{- end }}
{{- end }}

{{/*
Database secret name
*/}}
{{- define "tsm.databaseSecretName" -}}
{{- if .Values.externalDatabase.existingSecret }}
{{- .Values.externalDatabase.existingSecret }}
{{- else }}
{{- include "tsm.fullname" . }}
{{- end }}
{{- end }}

{{/*
Backend service name (frontend nginx proxy + HTTPRoute target)
*/}}
{{- define "tsm.backendServiceName" -}}
{{- printf "%s-backend" (include "tsm.fullname" .) }}
{{- end }}

{{/*
Shared backend pod spec fragments: envFrom for config + secrets
*/}}
{{- define "tsm.backendEnvFrom" -}}
envFrom:
  - configMapRef:
      name: {{ include "tsm.fullname" . }}-config
  - secretRef:
      name: {{ include "tsm.secretName" . }}
{{- if .Values.externalDatabase.existingSecret }}
  - secretRef:
      name: {{ .Values.externalDatabase.existingSecret }}
{{- end }}
{{- end }}
