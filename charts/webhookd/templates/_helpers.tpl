{{/*
Expand the chart name. Use override when set; otherwise the bare chart name.
*/}}
{{- define "webhookd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Generate a fully-qualified app name. Honor `fullnameOverride` first; fall
back to `<release>-<chart>` (or just the release name when it already
contains the chart name).
*/}}
{{- define "webhookd.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart label value (`<name>-<version>`).
*/}}
{{- define "webhookd.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard labels applied to every chart-managed resource.
*/}}
{{- define "webhookd.labels" -}}
helm.sh/chart: {{ include "webhookd.chart" . }}
{{ include "webhookd.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — the subset that may never change post-install.
*/}}
{{- define "webhookd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "webhookd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name — chart-generated default unless `serviceAccount.name` is set.
*/}}
{{- define "webhookd.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "webhookd.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Target namespace where the Role/RoleBinding land. Always
`rbac.targetNamespace`; the helper exists to keep templates DRY and
to centralize a single override point if we later support multiple
target namespaces.
*/}}
{{- define "webhookd.targetNamespace" -}}
{{- required "rbac.targetNamespace must be set" .Values.rbac.targetNamespace -}}
{{- end -}}

{{/*
Comma-joined list of provider names whose `<provider>.enabled=true`. The
output is consumed by the `WEBHOOK_PROVIDERS` env var. Today only `jsm`
is supported; future providers extend the conditional chain.
*/}}
{{- define "webhookd.enabledProviders" -}}
{{- $providers := list -}}
{{- if .Values.jsm.enabled -}}
{{- $providers = append $providers "jsm" -}}
{{- end -}}
{{- join "," $providers -}}
{{- end -}}

{{/*
Name of the Secret that carries the HMAC signing key. Resolves either
the chart-rendered Secret (`createSecret=true`) or the user-provided
existingSecret. Templates consume it via `secretKeyRef.name`.
*/}}
{{- define "webhookd.signingSecretName" -}}
{{- if .Values.signing.createSecret -}}
{{- printf "%s-signing" (include "webhookd.fullname" .) -}}
{{- else -}}
{{- required "signing.existingSecret must be set when signing.createSecret=false" .Values.signing.existingSecret -}}
{{- end -}}
{{- end -}}

{{/*
Key in the signing Secret. `webhookSecret` for chart-rendered secrets;
`signing.existingSecretKey` (default `webhookSecret`) for external secrets.
*/}}
{{- define "webhookd.signingSecretKey" -}}
{{- if .Values.signing.createSecret -}}
webhookSecret
{{- else -}}
{{- default "webhookSecret" .Values.signing.existingSecretKey -}}
{{- end -}}
{{- end -}}
