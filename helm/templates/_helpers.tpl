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

{{/* Labels applied to every SIPREC recorder resource. */}}
{{- define "siprec.labels" -}}
app.kubernetes.io/name: siprec-recorder
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: siprec-recorder
{{ include "siprec-stack.chartLabel" . }}
{{- end }}

{{/* Selector labels for SIPREC recorder pods. */}}
{{- define "siprec.selectorLabels" -}}
app.kubernetes.io/name: siprec-recorder
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/* Base name of SIPREC recorder resources. */}}
{{- define "siprec.fullname" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Stable, DNS-safe suffix for a configured recorder instance. */}}
{{- define "siprec.instanceName" -}}
{{- required "siprecRecorder.instances[].name is required" .name | lower | replace "_" "-" | trunc 30 | trimSuffix "-" }}
{{- end }}

{{/* Full name for an instance-scoped SIPREC recorder resource. */}}
{{- define "siprec.instanceFullname" -}}
{{- $root := .root -}}
{{- $instanceName := include "siprec.instanceName" .instance -}}
{{- printf "%s-%s" (include "siprec.fullname" $root) $instanceName | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Selector labels for one recorder Deployment. */}}
{{- define "siprec.instanceSelectorLabels" -}}
{{ include "siprec.selectorLabels" .root }}
siprec-stack/recorder-instance: {{ include "siprec.instanceName" .instance | quote }}
{{- end }}

{{/* Name of the PVC used by one recorder Deployment. */}}
{{- define "siprec.instancePVCName" -}}
{{- printf "%s-recordings" (include "siprec.instanceFullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Name of the Kubernetes ServiceAccount used by recorder pods. */}}
{{- define "siprec.serviceAccountName" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Kamailio resource name. */}}
{{- define "kamailio.fullname" -}}
{{- printf "%s-kamailio" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Labels applied to Kamailio resources. */}}
{{- define "kamailio.labels" -}}
app.kubernetes.io/name: kamailio
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: sip-proxy
{{ include "siprec-stack.chartLabel" . }}
{{- end }}

{{/* Immutable Kamailio selector labels. */}}
{{- define "kamailio.selectorLabels" -}}
app.kubernetes.io/name: kamailio
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}
