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
Full name of the SIPREC recorder DaemonSet / Service.
*/}}
{{- define "siprec.fullname" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Stable, DNS-safe suffix for a configured recorder node.
*/}}
{{- define "siprec.nodeName" -}}
{{- required "siprecRecorder.nodes[].name is required" .name | lower | replace "_" "-" | trunc 30 | trimSuffix "-" }}
{{- end }}

{{/*
Full name for a node-scoped SIPREC recorder resource.
*/}}
{{- define "siprec.nodeFullname" -}}
{{- $root := .root -}}
{{- $nodeName := include "siprec.nodeName" .node -}}
{{- printf "%s-%s" (include "siprec.fullname" $root) $nodeName | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels for a single node-scoped recorder pod.
*/}}
{{- define "siprec.nodeSelectorLabels" -}}
{{ include "siprec.selectorLabels" .root }}
siprec-stack/recorder-node: {{ include "siprec.nodeName" .node | quote }}
{{- end }}

{{/*
Name of the Kubernetes ServiceAccount used by the SIPREC recorder pods.
*/}}
{{- define "siprec.serviceAccountName" -}}
{{- printf "%s-siprec-recorder" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
