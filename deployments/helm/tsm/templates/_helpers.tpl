{{/*
Common labels
*/}}
{{- define "tsm.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Backend selector labels
*/}}
{{- define "tsm.backendSelectorLabels" -}}
app: tsm
component: backend
{{- end }}

{{/*
Frontend selector labels
*/}}
{{- define "tsm.frontendSelectorLabels" -}}
app: tsm
component: frontend
{{- end }}
