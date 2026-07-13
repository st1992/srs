{{/*
Expand the release namespace, allowing an override via .Values.namespaceOverride.
*/}}
{{- define "streamlink.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{/*
Chart label, shared by all resources.
*/}}
{{- define "streamlink.chartLabel" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end }}

{{/* Labels applied to every recorder resource. */}}
{{- define "recorder.labels" -}}
app.kubernetes.io/name: cx-streamlink-rec
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: recorder
{{ include "streamlink.chartLabel" . }}
{{- end }}

{{/* Selector labels for recorder pods. */}}
{{- define "recorder.selectorLabels" -}}
app.kubernetes.io/name: cx-streamlink-rec
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/* Fixed base name of recorder resources. */}}
{{- define "recorder.fullname" -}}
{{- print "cx-streamlink-rec" }}
{{- end }}

{{/* Stable, DNS-safe suffix for a configured recorder instance. */}}
{{- define "recorder.instanceName" -}}
{{- required "recorder.instances[].name is required" .name | lower | replace "_" "-" | trunc 30 | trimSuffix "-" }}
{{- end }}

{{/* Full name for an instance-scoped recorder resource. */}}
{{- define "recorder.instanceFullname" -}}
{{- $root := .root -}}
{{- $instanceName := include "recorder.instanceName" .instance -}}
{{- $baseName := include "recorder.fullname" $root -}}
{{- printf "%s-%s" $baseName $instanceName | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Selector labels for one recorder Deployment. */}}
{{- define "recorder.instanceSelectorLabels" -}}
{{ include "recorder.selectorLabels" .root }}
cx-streamlink/recorder-instance: {{ include "recorder.instanceName" .instance | quote }}
{{- end }}

{{/* Name of the PVC used by one recorder Deployment. */}}
{{- define "recorder.instancePVCName" -}}
{{- printf "%s-recordings" (include "recorder.instanceFullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Name of the Kubernetes ServiceAccount used by recorder pods. */}}
{{- define "recorder.serviceAccountName" -}}
{{- print "cx-streamlink-rec" }}
{{- end }}

{{/* Fixed proxy resource name. */}}
{{- define "proxy.fullname" -}}
{{- print "cx-streamlink-proxy" }}
{{- end }}

{{/* Labels applied to proxy resources. */}}
{{- define "proxy.labels" -}}
app.kubernetes.io/name: cx-streamlink-proxy
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: proxy
{{ include "streamlink.chartLabel" . }}
{{- end }}

{{/* Immutable proxy selector labels. */}}
{{- define "proxy.selectorLabels" -}}
app.kubernetes.io/name: cx-streamlink-proxy
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}
