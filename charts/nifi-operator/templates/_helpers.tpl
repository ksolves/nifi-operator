{{- define "nifi-operator.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nifi-operator.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nifi-operator.labels" -}}
app.kubernetes.io/name: {{ include "nifi-operator.name" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nifi-operator.selectorLabels" -}}
control-plane: controller-manager
app.kubernetes.io/name: {{ include "nifi-operator.name" . }}
{{- end -}}

{{- define "nifi-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "nifi-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- /* repository:tag[@digest]; the tag defaults to <defaultTagPrefix>-<Chart appVersion>. */ -}}
{{- define "nifi-operator.image" -}}
{{- $tag := .image.tag | default (printf "%s-%s" .defaultTagPrefix .root.Chart.AppVersion) -}}
{{- .image.repository }}:{{ $tag }}{{ with .image.digest }}@{{ . }}{{ end -}}
{{- end -}}

{{- define "nifi-operator.webhookCertManager" -}}
{{- $enabled := .Values.webhook.certManager.enabled -}}
{{- if kindIs "bool" $enabled -}}
{{- $enabled -}}
{{- else -}}
{{- .Capabilities.APIVersions.Has "cert-manager.io/v1" -}}
{{- end -}}
{{- end -}}
