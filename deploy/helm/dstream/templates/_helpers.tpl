{{- define "dstream.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "dstream.labels" -}}
app.kubernetes.io/name: dstream
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: dstream-{{ .Chart.Version }}
{{- end -}}

{{- define "dstream.selectorLabels" -}}
app.kubernetes.io/name: dstream
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "dstream.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "dstream.webImage" -}}
{{ .Values.web.image.repository }}:{{ .Values.web.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "dstream.serviceAccountName" -}}
{{ include "dstream.fullname" . }}
{{- end -}}
