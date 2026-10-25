{{- define "mnemo.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}{{ .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}

{{- define "mnemo.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "mnemo.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "mnemo.hasHub" -}}{{ if has .Values.mode (list "all" "hub") }}true{{ end }}{{- end }}
{{- define "mnemo.hasAgent" -}}{{ if has .Values.mode (list "all" "agent") }}true{{ end }}{{- end }}
{{- define "mnemo.tokenSecret" -}}{{ .Values.token.existingSecret | default (printf "%s-token" (include "mnemo.fullname" .)) }}{{- end }}

{{/* Fail early with a readable message instead of a crash-looping pod. */}}
{{- define "mnemo.validate" -}}
{{- if not (has .Values.mode (list "all" "hub" "agent")) }}{{ fail "mode must be all, hub, or agent" }}{{ end }}
{{- if and (include "mnemo.hasAgent" .) (not .Values.agent.cluster) }}{{ fail "agent.cluster is required: the name this cluster gets in mnemo" }}{{ end }}
{{- if and (eq .Values.mode "agent") (not .Values.agent.hubURL) }}{{ fail "agent.hubURL is required in mode agent" }}{{ end }}
{{- if and (eq .Values.mode "agent") (not .Values.token.existingSecret) }}{{ fail "token.existingSecret is required in mode agent: it must hold the hub's token" }}{{ end }}
{{- if and (include "mnemo.hasAgent" .) .Values.agent.audit.enabled (not .Values.agent.audit.existingSecret) }}{{ fail "agent.audit.existingSecret is required: a Secret with key token, also used in the API server's webhook config" }}{{ end }}
{{- range .Values.agent.watch }}{{ if or (eq . "v1/secrets") (hasSuffix "/secrets" .) }}{{ fail "secrets are never recorded: mnemo would store their values" }}{{ end }}{{ end }}
{{- with .Values.hub.auth }}
{{- if not (has .mode (list "none" "oidc" "proxy")) }}{{ fail "hub.auth.mode must be none, oidc, or proxy" }}{{ end }}
{{- if and (ne .mode "none") (not .policy) }}{{ fail "hub.auth.policy is required with oidc/proxy: without it nobody could see anything" }}{{ end }}
{{- if and (eq .mode "oidc") (not (and .oidc.issuer .oidc.clientID .oidc.redirectURL .oidc.existingSecret)) }}{{ fail "hub.auth.oidc needs issuer, clientID, redirectURL and existingSecret" }}{{ end }}
{{- if and (eq .mode "proxy") (not .proxy.trustedCIDRs) }}{{ fail "hub.auth.proxy.trustedCIDRs is required: otherwise anyone reaching the hub could claim any identity" }}{{ end }}
{{- end }}
{{- end }}
