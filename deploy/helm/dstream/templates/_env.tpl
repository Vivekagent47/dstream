{{- define "dstream.secretName" -}}
{{- if .Values.secrets.existingSecret -}}{{ .Values.secrets.existingSecret }}{{- else -}}{{ include "dstream.fullname" . }}-secret{{- end -}}
{{- end -}}

{{- define "dstream.publicBaseURL" -}}
{{- if .Values.config.publicBaseURL -}}{{ .Values.config.publicBaseURL }}
{{- else -}}{{- if not .Values.ingress.host -}}{{ fail "ingress.host is required (or set config.publicBaseURL) so base URLs resolve" }}{{- end -}}https://{{ .Values.ingress.host }}{{- end -}}
{{- end -}}

{{- define "dstream.appBaseURL" -}}
{{- if .Values.config.appBaseURL -}}{{ .Values.config.appBaseURL }}{{- else -}}{{ include "dstream.publicBaseURL" . }}{{- end -}}
{{- end -}}

{{- define "dstream.cookieSecure" -}}
{{- if ne (toString .Values.config.cookieSecure) "" -}}{{ .Values.config.cookieSecure }}{{- else -}}{{ .Values.ingress.tls.enabled }}{{- end -}}
{{- end -}}

{{- define "dstream.redisAddr" -}}
{{- if .Values.redis.enabled -}}{{ include "dstream.fullname" . }}-redis:6379{{- else -}}
{{- if not .Values.redis.addr -}}{{ fail "redis.enabled=false requires redis.addr" }}{{- end -}}{{ .Values.redis.addr }}{{- end -}}
{{- end -}}

{{- define "dstream.envFrom" -}}
- configMapRef:
    name: {{ include "dstream.fullname" . }}-config
- secretRef:
    name: {{ include "dstream.secretName" . }}
{{- end -}}
