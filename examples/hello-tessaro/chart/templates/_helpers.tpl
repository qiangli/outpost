{{- define "hello-tessaro.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hello-tessaro.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "hello-tessaro.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hello-tessaro.labels" -}}
app.kubernetes.io/name: {{ include "hello-tessaro.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
dhnt.io/environment: {{ .Values.env | quote }}
{{- end -}}

{{- define "hello-tessaro.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hello-tessaro.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The Secret name holding the SSO secret: an existing (Sealed) Secret, else the chart-created one. */}}
{{- define "hello-tessaro.ssoSecretName" -}}
{{- if .Values.ssoSecretName -}}
{{- .Values.ssoSecretName -}}
{{- else -}}
{{- printf "%s-sso" (include "hello-tessaro.fullname" .) -}}
{{- end -}}
{{- end -}}
