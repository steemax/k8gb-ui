{{- define "k8gb-ui.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- define "k8gb-ui.fullname" -}}
{{- default (printf "%s-%s" .Release.Name (include "k8gb-ui.name" .)) .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- define "k8gb-ui.labels" -}}
app.kubernetes.io/name: {{ include "k8gb-ui.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
{{- define "k8gb-ui.agentSA" -}}
{{- default (printf "%s-agent" (include "k8gb-ui.fullname" .)) .Values.agent.serviceAccountName -}}
{{- end -}}
{{- define "k8gb-ui.webhookSA" -}}
{{- default (printf "%s-webhook" (include "k8gb-ui.fullname" .)) .Values.webhook.serviceAccountName -}}
{{- end -}}
{{- define "k8gb-ui.webhookName" -}}
{{- default (printf "%s-webhook" (include "k8gb-ui.fullname" .)) .Values.traefik.webhookConfigurationName -}}
{{- end -}}
{{- define "k8gb-ui.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile: {type: RuntimeDefault}
{{- end -}}
{{- define "k8gb-ui.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end -}}
{{- define "k8gb-ui.consoleConfig" -}}
listen: "0.0.0.0:8080"
writesEnabled: {{ .Values.console.writesEnabled }}
auth:
  {{- toYaml .Values.console.auth | nindent 2 }}
clusters:
  {{- toYaml .Values.console.clusters | nindent 2 }}
{{- end -}}
