{{- define "coredns-crd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "coredns-crd.fullname" -}}
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

{{- define "coredns-crd.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "coredns-crd.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "coredns-crd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "coredns-crd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
k8s-app: kube-dns
{{- end -}}

{{- define "coredns-crd.serviceAccountName" -}}
{{ include "coredns-crd.fullname" . }}
{{- end -}}

{{- define "coredns-crd.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{- define "coredns-crd.leaseNamespace" -}}
{{- default .Release.Namespace .Values.leaderElection.namespace -}}
{{- end -}}

{{/*
  forwardUpstreams: returns a space-joined list of upstream addresses, or
  empty string. Resolution order: explicit `corefile.forward.upstreams`
  list (if non-empty) wins; otherwise fall back to legacy
  `corefile.forward.upstream` string (treated as a single-element list).
  Returns "" if both are empty/null/absent.
*/}}
{{- define "coredns-crd.forwardUpstreams" -}}
{{- $list := .Values.corefile.forward.upstreams | default (list) -}}
{{- if not (empty $list) -}}
{{- join " " $list -}}
{{- else if .Values.corefile.forward.upstream -}}
{{- .Values.corefile.forward.upstream -}}
{{- end -}}
{{- end -}}

{{/*
  forwardEnabled: returns "true" iff forwardUpstreams resolves to a
  non-empty value. Used to gate emission of the `forward` plugin block.
*/}}
{{- define "coredns-crd.forwardEnabled" -}}
{{- if include "coredns-crd.forwardUpstreams" . -}}true{{- end -}}
{{- end -}}

{{/*
  kubernetesEnabled: returns "true" iff the Corefile should include the
  kubernetes plugin block. Disabled in standalone installs (cluster's
  existing CoreDNS owns cluster.local).
*/}}
{{- define "coredns-crd.kubernetesEnabled" -}}
{{- if .Values.corefile.kubernetes.enabled -}}true{{- end -}}
{{- end -}}
