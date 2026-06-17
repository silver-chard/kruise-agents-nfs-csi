{{- define "kruise-agents-nfs-csi-wrapper-demo.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kruise-agents-nfs-csi-wrapper-demo.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "kruise-agents-nfs-csi-wrapper-demo.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
