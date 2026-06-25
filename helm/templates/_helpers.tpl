{{/*
Expand the release namespace, allowing an override via .Values.namespaceOverride.
*/}}
{{- define "siprec-stack.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{/*
Chart label, shared by all resources.
*/}}
{{- define "siprec-stack.chartLabel" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end }}

{{/*
Labels applied to every Kamailio resource.
*/}}
{{- define "kamailio.labels" -}}
app.kubernetes.io/name: kamailio
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: siprec-proxy
{{ include "siprec-stack.chartLabel" . }}
{{- end }}

{{/*
Selector labels for Kamailio pods (stable subset, never change after first deploy).
*/}}
{{- define "kamailio.selectorLabels" -}}
app.kubernetes.io/name: kamailio
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
Labels applied to every SIPREC recorder resource.
*/}}
{{- define "siprec.labels" -}}
app.kubernetes.io/name: siprec-recorder
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: siprec-recorder
{{ include "siprec-stack.chartLabel" . }}
{{- end }}

{{/*
Selector labels for SIPREC recorder pods.
*/}}
{{- define "siprec.selectorLabels" -}}
app.kubernetes.io/name: siprec-recorder
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
Full name of the Kamailio Deployment / Service.
*/}}
{{- define "kamailio.fullname" -}}
{{- printf "%s-kamailio" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Full name of the SIPREC recorder DaemonSet / headless Service.
*/}}
{{- define "siprec.fullname" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name of the headless Service used by the Kamailio init-container to discover
recorder pod IPs.
*/}}
{{- define "siprec.headlessServiceName" -}}
{{- printf "%s-siprec-recorder-headless" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name of the Kubernetes ServiceAccount used by the SIPREC recorder pods.
*/}}
{{- define "siprec.serviceAccountName" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
