{{- define "kruise-agents-nfs-csi.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "kruise-agents-nfs-csi.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.labels" -}}
app.kubernetes.io/name: {{ include "kruise-agents-nfs-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "kruise-agents-nfs-csi.controllerServiceAccount" -}}
{{- default (printf "%s-controller" (include "kruise-agents-nfs-csi.fullname" .)) .Values.controller.serviceAccountName -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.nodeServiceAccount" -}}
{{- default (printf "%s-node" (include "kruise-agents-nfs-csi.fullname" .)) .Values.node.serviceAccountName -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.wrapperImage" -}}
{{- printf "%s:%s" .Values.images.wrapper.repository .Values.images.wrapper.tag -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi.image" -}}
{{- printf "%s:%s" .repository .tag -}}
{{- end -}}
