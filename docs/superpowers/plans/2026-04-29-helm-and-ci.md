# Helm Chart + CI + Signed Releases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `coredns-crd` installable by anyone via `helm install oci://ghcr.io/...` with cosign-verifiable provenance, gated by GitHub Actions tests.

**Architecture:** Helm chart at `deploy/helm/coredns-crd/` templates every existing manifest (CRD via `crds/`, RBAC, SA, Deployment, Service, ConfigMap, PDB). Two GitHub Actions workflows: `ci.yml` runs `go test -race`, `helm lint`, and `helm template | kubeconform` on PRs/main; `release.yml` triggers on `v*.*.*` tags, builds + cosign-signs the image and the chart OCI artifact, and creates a GitHub Release.

**Tech Stack:** Helm v3, GitHub Actions, cosign (keyless via OIDC), kubeconform, ghcr.io.

---

## File structure

| File | Action | Responsibility |
|-|-|-|
| `deploy/helm/coredns-crd/Chart.yaml` | new | Chart metadata: name, version, appVersion, kubeVersion, description. |
| `deploy/helm/coredns-crd/values.yaml` | new | Documented defaults; every knob the spec calls out. |
| `deploy/helm/coredns-crd/README.md` | new | Install + verify + values doc generated from values.yaml comments. |
| `deploy/helm/coredns-crd/templates/_helpers.tpl` | new | `coredns-crd.fullname`, common labels, selector labels. |
| `deploy/helm/coredns-crd/templates/serviceaccount.yaml` | new | SA in release namespace. |
| `deploy/helm/coredns-crd/templates/clusterrole.yaml` | new | crd plugin + kubernetes plugin + events RBAC. |
| `deploy/helm/coredns-crd/templates/clusterrolebinding.yaml` | new | Binds ClusterRole to SA. |
| `deploy/helm/coredns-crd/templates/role-leader.yaml` | new | Lease lock Role in lease namespace. |
| `deploy/helm/coredns-crd/templates/rolebinding-leader.yaml` | new | Binds lease Role to SA. |
| `deploy/helm/coredns-crd/templates/configmap.yaml` | new | Templated Corefile from `.Values.corefile`. |
| `deploy/helm/coredns-crd/templates/deployment.yaml` | new | Deployment with env, probes, security context, topology spread. |
| `deploy/helm/coredns-crd/templates/service.yaml` | new | kube-dns Service with optional clusterIP. |
| `deploy/helm/coredns-crd/templates/poddisruptionbudget.yaml` | new | PDB with `minAvailable` from values. |
| `deploy/helm/coredns-crd/templates/NOTES.txt` | new | Post-install hints (verification command, dig snippet). |
| `deploy/helm/coredns-crd/crds/dnsslice.yaml` | new | Copy of `config/crd/dns.coredns-crd.io_dnsslices.yaml`. |
| `.github/workflows/ci.yml` | new | Test + lint + chart-render workflow. |
| `.github/workflows/release.yml` | new | Tag-driven build + sign + push + Release. |

---

## Task 1: Chart skeleton (`Chart.yaml` + `values.yaml`)

**Files:**
- Create: `deploy/helm/coredns-crd/Chart.yaml`
- Create: `deploy/helm/coredns-crd/values.yaml`

- [ ] **Step 1.1: Write `Chart.yaml`**

```yaml
apiVersion: v2
name: coredns-crd
description: |
  CoreDNS as cluster DNS, with DNS-as-code via the DNSSlice CRD.
  Resolves DNSSlice records via a custom CoreDNS plugin and chains
  the standard kubernetes plugin for cluster.local resolution.
type: application
version: 0.1.0
appVersion: "v0.1.0"
kubeVersion: ">=1.28.0-0"
keywords: [dns, coredns, crd]
home: https://github.com/hbinhng/coredns-crd
sources:
  - https://github.com/hbinhng/coredns-crd
maintainers:
  - name: hbinhng
```

- [ ] **Step 1.2: Write `values.yaml`**

```yaml
# Default values for coredns-crd. Override with --set or -f.

nameOverride: ""
fullnameOverride: ""

replicaCount: 2

image:
  repository: ghcr.io/hbinhng/coredns-crd
  # tag defaults to .Chart.AppVersion when empty
  tag: ""
  pullPolicy: IfNotPresent

imagePullSecrets: []

resources:
  requests:
    cpu: 100m
    memory: 70Mi
  limits:
    memory: 200Mi

service:
  type: ClusterIP
  # When replacing kube-dns, set clusterIP explicitly so kubelet's
  # --cluster-dns flag points at us. e.g. 10.96.0.10 (kubeadm) or
  # 10.43.0.10 (k3s). Empty = let the API server allocate.
  clusterIP: ""
  ports:
    dns: 53
    dnsTcp: 53
    metrics: 9153
  annotations:
    prometheus.io/port: "9153"
    prometheus.io/scrape: "true"

leaderElection:
  enabled: true
  # namespace defaults to release namespace via downward API
  namespace: ""
  leaseName: coredns-crd-leader

corefile:
  zones: [".:53"]
  errors: true
  health:
    enabled: true
    lameduck: 5s
  ready: true
  crd:
    fallthrough: true
  kubernetes:
    enabled: true
    cluster: cluster.local
    pods: insecure
    fallthroughZones: ["in-addr.arpa", "ip6.arpa"]
    ttl: 30
  prometheus:
    listen: ":9153"
  forward:
    upstream: /etc/resolv.conf
    maxConcurrent: 1000
  cache:
    ttl: 30
  loop: true
  reload: true
  loadbalance: true

podDisruptionBudget:
  enabled: true
  minAvailable: 1

topologySpreadConstraints:
  enabled: true
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway

tolerations:
  - key: CriticalAddonsOnly
    operator: Exists
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule

nodeSelector:
  kubernetes.io/os: linux

priorityClassName: system-cluster-critical
```

- [ ] **Step 1.3: Verify `helm lint` passes on the skeleton**

Run: `helm lint deploy/helm/coredns-crd`
Expected: `1 chart(s) linted, 0 chart(s) failed` (warnings about missing icon are OK).

- [ ] **Step 1.4: Commit**

```
git add deploy/helm/coredns-crd/Chart.yaml deploy/helm/coredns-crd/values.yaml
git commit -m "feat(helm): chart skeleton (Chart.yaml + values.yaml)"
```

---

## Task 2: Helpers + CRD copy

**Files:**
- Create: `deploy/helm/coredns-crd/templates/_helpers.tpl`
- Create: `deploy/helm/coredns-crd/crds/dnsslice.yaml`

- [ ] **Step 2.1: Write `_helpers.tpl`**

```yaml
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
k8s-app: kube-dns
kubernetes.io/name: CoreDNS
{{- end -}}

{{- define "coredns-crd.selectorLabels" -}}
k8s-app: kube-dns
app.kubernetes.io/instance: {{ .Release.Name }}
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
```

Note on selector labels: the `app.kubernetes.io/instance` label scopes the
selector to a single Helm release; `k8s-app: kube-dns` is preserved so the
existing kubelet conventions for cluster DNS continue to apply.

- [ ] **Step 2.2: Copy CRD into `crds/`**

```
cp config/crd/dns.coredns-crd.io_dnsslices.yaml deploy/helm/coredns-crd/crds/dnsslice.yaml
```

The CRD lives in `crds/` (not `templates/`) so Helm 3 installs it once on
`helm install` and never touches it on `upgrade` or `uninstall`. CRD updates
are applied manually with `kubectl apply -f crds/dnsslice.yaml`.

- [ ] **Step 2.3: Verify lint after both files**

Run: `helm lint deploy/helm/coredns-crd`
Expected: still passes (no templates yet, so no rendering errors).

- [ ] **Step 2.4: Commit**

```
git add deploy/helm/coredns-crd/templates/_helpers.tpl deploy/helm/coredns-crd/crds/
git commit -m "feat(helm): _helpers.tpl + CRD in crds/ for install-once semantics"
```

---

## Task 3: ServiceAccount + ClusterRole + Role

**Files:**
- Create: `deploy/helm/coredns-crd/templates/serviceaccount.yaml`
- Create: `deploy/helm/coredns-crd/templates/clusterrole.yaml`
- Create: `deploy/helm/coredns-crd/templates/clusterrolebinding.yaml`
- Create: `deploy/helm/coredns-crd/templates/role-leader.yaml`
- Create: `deploy/helm/coredns-crd/templates/rolebinding-leader.yaml`

- [ ] **Step 3.1: ServiceAccount**

`deploy/helm/coredns-crd/templates/serviceaccount.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "coredns-crd.serviceAccountName" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
```

- [ ] **Step 3.2: ClusterRole**

`deploy/helm/coredns-crd/templates/clusterrole.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
rules:
  # crd plugin
  - apiGroups: ["dns.coredns-crd.io"]
    resources: ["dnsslices"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["dns.coredns-crd.io"]
    resources: ["dnsslices/status"]
    verbs: ["get", "patch", "update"]
  {{- if .Values.corefile.kubernetes.enabled }}
  # kubernetes plugin
  - apiGroups: [""]
    resources: ["endpoints", "services", "pods", "namespaces"]
    verbs: ["list", "watch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
  {{- end }}
  # Conflict transition Events
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

- [ ] **Step 3.3: ClusterRoleBinding**

`deploy/helm/coredns-crd/templates/clusterrolebinding.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "coredns-crd.fullname" . }}
subjects:
  - kind: ServiceAccount
    name: {{ include "coredns-crd.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
```

- [ ] **Step 3.4: Lease Role + RoleBinding (only when leader election enabled)**

`deploy/helm/coredns-crd/templates/role-leader.yaml`:

```yaml
{{- if .Values.leaderElection.enabled }}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "coredns-crd.fullname" . }}-leader
  namespace: {{ include "coredns-crd.leaseNamespace" . }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update", "patch"]
{{- end }}
```

`deploy/helm/coredns-crd/templates/rolebinding-leader.yaml`:

```yaml
{{- if .Values.leaderElection.enabled }}
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "coredns-crd.fullname" . }}-leader
  namespace: {{ include "coredns-crd.leaseNamespace" . }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "coredns-crd.fullname" . }}-leader
subjects:
  - kind: ServiceAccount
    name: {{ include "coredns-crd.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
{{- end }}
```

- [ ] **Step 3.5: Verify rendering**

Run: `helm template coredns-crd deploy/helm/coredns-crd --namespace kube-system`
Expected: emits SA, ClusterRole, ClusterRoleBinding, Role, RoleBinding with correct names. No template errors.

- [ ] **Step 3.6: Commit**

```
git add deploy/helm/coredns-crd/templates/
git commit -m "feat(helm): SA, ClusterRole, RoleBinding for crd plugin + leader election"
```

---

## Task 4: ConfigMap (templated Corefile)

**Files:**
- Create: `deploy/helm/coredns-crd/templates/configmap.yaml`

- [ ] **Step 4.1: Write the Corefile template**

`deploy/helm/coredns-crd/templates/configmap.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
data:
  Corefile: |
    {{- range .Values.corefile.zones }}
    {{ . }} {
        {{- if $.Values.corefile.errors }}
        errors
        {{- end }}
        {{- if $.Values.corefile.health.enabled }}
        health {
          lameduck {{ $.Values.corefile.health.lameduck }}
        }
        {{- end }}
        {{- if $.Values.corefile.ready }}
        ready
        {{- end }}
        crd {
          {{- if $.Values.corefile.crd.fallthrough }}
          fallthrough
          {{- end }}
          {{- if not $.Values.leaderElection.enabled }}
          leader_election {
            disable
          }
          {{- else if or $.Values.leaderElection.namespace (ne $.Values.leaderElection.leaseName "coredns-crd-leader") }}
          leader_election {
            {{- with $.Values.leaderElection.namespace }}
            namespace {{ . }}
            {{- end }}
            {{- if ne $.Values.leaderElection.leaseName "coredns-crd-leader" }}
            lease_name {{ $.Values.leaderElection.leaseName }}
            {{- end }}
          }
          {{- end }}
        }
        {{- if $.Values.corefile.kubernetes.enabled }}
        kubernetes {{ $.Values.corefile.kubernetes.cluster }} {{ join " " $.Values.corefile.kubernetes.fallthroughZones }} {
          pods {{ $.Values.corefile.kubernetes.pods }}
          fallthrough {{ join " " $.Values.corefile.kubernetes.fallthroughZones }}
          ttl {{ $.Values.corefile.kubernetes.ttl }}
        }
        {{- end }}
        prometheus {{ $.Values.corefile.prometheus.listen }}
        forward . {{ $.Values.corefile.forward.upstream }} {
          max_concurrent {{ $.Values.corefile.forward.maxConcurrent }}
        }
        cache {{ $.Values.corefile.cache.ttl }}
        {{- if $.Values.corefile.loop }}
        loop
        {{- end }}
        {{- if $.Values.corefile.reload }}
        reload
        {{- end }}
        {{- if $.Values.corefile.loadbalance }}
        loadbalance
        {{- end }}
    }
    {{- end }}
```

Note on `crd` block conditionals: the inner `leader_election { ... }` block
only renders when something actually changes — keeps the rendered Corefile
minimal in the common case (default namespace + default lease name).

- [ ] **Step 4.2: Verify default render**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  | sed -n '/Corefile:/,/^---/p'
```
Expected: a clean Corefile with `crd { fallthrough }` (no inner block since
defaults), the `kubernetes` plugin, `prometheus :9153`, etc.

- [ ] **Step 4.3: Verify `leader_election` disabled render**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set leaderElection.enabled=false \
  | grep -A2 'crd {'
```
Expected: emits `leader_election { disable }` inside the `crd` block.

- [ ] **Step 4.4: Commit**

```
git add deploy/helm/coredns-crd/templates/configmap.yaml
git commit -m "feat(helm): templated Corefile ConfigMap"
```

---

## Task 5: Deployment + Service + PDB

**Files:**
- Create: `deploy/helm/coredns-crd/templates/deployment.yaml`
- Create: `deploy/helm/coredns-crd/templates/service.yaml`
- Create: `deploy/helm/coredns-crd/templates/poddisruptionbudget.yaml`

- [ ] **Step 5.1: Deployment**

`deploy/helm/coredns-crd/templates/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
  selector:
    matchLabels: {{- include "coredns-crd.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels: {{- include "coredns-crd.labels" . | nindent 8 }}
      annotations:
        checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
    spec:
      {{- with .Values.priorityClassName }}
      priorityClassName: {{ . }}
      {{- end }}
      serviceAccountName: {{ include "coredns-crd.serviceAccountName" . }}
      {{- with .Values.imagePullSecrets }}
      imagePullSecrets: {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- if .Values.topologySpreadConstraints.enabled }}
      topologySpreadConstraints:
        - maxSkew: {{ .Values.topologySpreadConstraints.maxSkew }}
          topologyKey: {{ .Values.topologySpreadConstraints.topologyKey }}
          whenUnsatisfiable: {{ .Values.topologySpreadConstraints.whenUnsatisfiable }}
          labelSelector:
            matchLabels: {{- include "coredns-crd.selectorLabels" . | nindent 14 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations: {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.nodeSelector }}
      nodeSelector: {{- toYaml . | nindent 8 }}
      {{- end }}
      containers:
        - name: coredns
          image: {{ include "coredns-crd.image" . }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args: ["-conf", "/etc/coredns/Corefile"]
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          volumeMounts:
            - name: config-volume
              mountPath: /etc/coredns
              readOnly: true
          ports:
            - containerPort: {{ .Values.service.ports.dns }}
              name: dns
              protocol: UDP
            - containerPort: {{ .Values.service.ports.dnsTcp }}
              name: dns-tcp
              protocol: TCP
            - containerPort: {{ .Values.service.ports.metrics }}
              name: metrics
              protocol: TCP
          resources: {{- toYaml .Values.resources | nindent 12 }}
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              add: [NET_BIND_SERVICE]
              drop: [all]
            readOnlyRootFilesystem: true
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
              scheme: HTTP
            initialDelaySeconds: 60
            timeoutSeconds: 5
            successThreshold: 1
            failureThreshold: 5
          readinessProbe:
            httpGet:
              path: /ready
              port: 8181
              scheme: HTTP
      dnsPolicy: Default
      volumes:
        - name: config-volume
          configMap:
            name: {{ include "coredns-crd.fullname" . }}
            items:
              - key: Corefile
                path: Corefile
```

The `checksum/config` annotation forces a pod restart whenever the Corefile
changes — Helm won't otherwise notice a ConfigMap-only change.

- [ ] **Step 5.2: Service**

`deploy/helm/coredns-crd/templates/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "coredns-crd.labels" . | nindent 4 }}
    kubernetes.io/cluster-service: "true"
  {{- with .Values.service.annotations }}
  annotations: {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ .Values.service.type }}
  {{- with .Values.service.clusterIP }}
  clusterIP: {{ . }}
  {{- end }}
  selector: {{- include "coredns-crd.selectorLabels" . | nindent 4 }}
  ports:
    - name: dns
      port: {{ .Values.service.ports.dns }}
      protocol: UDP
    - name: dns-tcp
      port: {{ .Values.service.ports.dnsTcp }}
      protocol: TCP
    - name: metrics
      port: {{ .Values.service.ports.metrics }}
      protocol: TCP
```

Note: the existing kube-dns Service in `deploy/coredns-deployment.yaml`
hard-codes `name: kube-dns`. With Helm, the Service name follows
`fullname` (e.g. `<release>-coredns-crd`). Operators replacing in-cluster
DNS need `fullnameOverride: kube-dns` to keep that exact name.

- [ ] **Step 5.3: PodDisruptionBudget**

`deploy/helm/coredns-crd/templates/poddisruptionbudget.yaml`:

```yaml
{{- if .Values.podDisruptionBudget.enabled }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
spec:
  minAvailable: {{ .Values.podDisruptionBudget.minAvailable }}
  selector:
    matchLabels: {{- include "coredns-crd.selectorLabels" . | nindent 6 }}
{{- end }}
```

- [ ] **Step 5.4: Verify rendering**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  > /tmp/rendered.yaml
wc -l /tmp/rendered.yaml
```
Expected: ~150-250 lines of YAML, no template errors, every kind present
(SA, ClusterRole, ClusterRoleBinding, Role, RoleBinding, ConfigMap,
Deployment, Service, PodDisruptionBudget).

- [ ] **Step 5.5: Validate against Kubernetes schemas**

Install kubeconform locally (or skip if not available; CI will catch):
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  | kubeconform -strict -summary -
```
Expected: `Summary: <N> resources found; Valid: <N>, Invalid: 0, ...`.

- [ ] **Step 5.6: Commit**

```
git add deploy/helm/coredns-crd/templates/
git commit -m "feat(helm): Deployment, Service, PodDisruptionBudget"
```

---

## Task 6: NOTES.txt + chart README

**Files:**
- Create: `deploy/helm/coredns-crd/templates/NOTES.txt`
- Create: `deploy/helm/coredns-crd/README.md`

- [ ] **Step 6.1: NOTES.txt (post-install hint)**

`deploy/helm/coredns-crd/templates/NOTES.txt`:

```text
coredns-crd installed.

Service: {{ include "coredns-crd.fullname" . }} (namespace {{ .Release.Namespace }})
{{- with .Values.service.clusterIP }}
ClusterIP: {{ . }}
{{- end }}

Verify resolution from a debug pod:

  kubectl run --rm -it dnstest --image=alpine:3.20 --restart=Never -- \
    sh -c "apk add --no-cache bind-tools && dig +short @{{ default "<service-cluster-ip>" .Values.service.clusterIP }} <name> <type>"

Apply an example DNSSlice:

  kubectl apply -f https://raw.githubusercontent.com/hbinhng/coredns-crd/main/config/example/dnsslice.yaml

Verify image signature:

  cosign verify {{ include "coredns-crd.image" . }} \
    --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

- [ ] **Step 6.2: README**

`deploy/helm/coredns-crd/README.md`:

```markdown
# coredns-crd Helm chart

CoreDNS as cluster DNS, with DNS-as-code via the DNSSlice CRD.

## Install

    helm install coredns-crd \
      oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
      --version 0.1.0 \
      --namespace kube-system \
      --create-namespace \
      --set service.clusterIP=10.96.0.10

The CRD `dnsslices.dns.coredns-crd.io` is installed automatically. Helm
3 will not modify it on `helm upgrade` — apply CRD changes manually.

## Verifying provenance

    cosign verify ghcr.io/hbinhng/coredns-crd:v0.1.0 \
      --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

## Replacing the cluster's existing DNS

To use this chart as the cluster's `kube-dns` Service, set:

    fullnameOverride: kube-dns
    service:
      clusterIP: <kubelet --cluster-dns IP>

Most distributions:
- kubeadm:  10.96.0.10
- k3s:      10.43.0.10
- minikube: see `kubectl get svc -n kube-system`

## Values

See `values.yaml` for the full set. Common knobs:

| Key | Default | Notes |
|-|-|-|
| `replicaCount` | 2 | Set ≥2 for HA. |
| `image.tag` | `""` (`.Chart.AppVersion`) | Override for testing. |
| `service.clusterIP` | `""` | Required when replacing kube-dns. |
| `leaderElection.enabled` | `true` | Set false only for replicaCount=1 dev. |
| `corefile.kubernetes.enabled` | `true` | Set false to skip cluster.local resolution. |
| `podDisruptionBudget.minAvailable` | 1 | DNS uptime guarantee during drains. |
```

- [ ] **Step 6.3: Verify lint after templates complete**

Run: `helm lint deploy/helm/coredns-crd`
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 6.4: Commit**

```
git add deploy/helm/coredns-crd/templates/NOTES.txt deploy/helm/coredns-crd/README.md
git commit -m "feat(helm): NOTES.txt + README with install + verify instructions"
```

---

## Task 7: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 7.1: Write the workflow**

`.github/workflows/ci.yml`:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read
  packages: write
  id-token: write

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
          cache: true
      - run: go vet ./...
      - run: go test -race -count=2 -coverprofile=cov.out ./...
      - name: Coverage gate (must stay 100% on protected packages)
        run: |
          set -euo pipefail
          for pkg in \
            github.com/hbinhng/coredns-crd/internal/index \
            github.com/hbinhng/coredns-crd/internal/leader \
            github.com/hbinhng/coredns-crd/internal/events; do
            cov=$(go tool cover -func=cov.out \
              | awk -v p="$pkg" 'index($1,p)==1 {n++; sum+=substr($3,1,length($3)-1)} END {if (n>0) printf "%.1f", sum/n; else print "0"}')
            echo "$pkg: $cov%"
            awk -v c="$cov" 'BEGIN { if (c+0 < 100.0) exit 1 }'
          done

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - uses: azure/setup-helm@v4
        with:
          version: v3.15.4
      - name: Install kubeconform
        run: |
          curl -fsSL https://github.com/yannh/kubeconform/releases/latest/download/kubeconform-linux-amd64.tar.gz \
            | tar xz -C /usr/local/bin kubeconform
      - run: helm lint deploy/helm/coredns-crd
      - name: Render + kubeconform
        run: |
          helm template coredns-crd deploy/helm/coredns-crd \
            --namespace kube-system \
            --set service.clusterIP=10.96.0.10 \
            > /tmp/rendered.yaml
          kubeconform -strict -summary /tmp/rendered.yaml

  build-main-image:
    needs: [test, lint]
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ghcr.io/${{ github.repository }}:main
          labels: |
            org.opencontainers.image.source=https://github.com/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.title=coredns-crd
```

- [ ] **Step 7.2: Validate with actionlint**

Install actionlint locally if available:
```
go install github.com/rhysd/actionlint/cmd/actionlint@latest
~/go/bin/actionlint .github/workflows/ci.yml
```
Expected: zero output (zero errors).

If actionlint not installed, run via Docker:
```
docker run --rm -v $PWD:/repo -w /repo rhysd/actionlint:latest -color
```

- [ ] **Step 7.3: Commit**

```
git add .github/workflows/ci.yml
git commit -m "ci: test + helm lint + kubeconform + build-main-image"
```

---

## Task 8: Release workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 8.1: Write the workflow**

`.github/workflows/release.yml`:

```yaml
name: Release

on:
  push:
    tags: ['v*.*.*']

permissions:
  contents: write
  packages: write
  id-token: write

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - run: go vet ./...
      - run: go test -race -count=2 ./...

  release:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: |
            ghcr.io/${{ github.repository }}:${{ github.ref_name }}
            ghcr.io/${{ github.repository }}:latest
          labels: |
            org.opencontainers.image.source=https://github.com/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.version=${{ github.ref_name }}
            org.opencontainers.image.title=coredns-crd

      - uses: sigstore/cosign-installer@v3
      - name: Sign image
        run: |
          cosign sign --yes \
            ghcr.io/${{ github.repository }}@${{ steps.build.outputs.digest }}

      - uses: azure/setup-helm@v4
        with:
          version: v3.15.4

      - name: Bump Chart.yaml + package + push
        run: |
          set -euo pipefail
          tag=${GITHUB_REF_NAME#v}
          sed -i "s/^version: .*/version: ${tag}/" deploy/helm/coredns-crd/Chart.yaml
          sed -i "s/^appVersion: .*/appVersion: \"${GITHUB_REF_NAME}\"/" deploy/helm/coredns-crd/Chart.yaml
          helm package deploy/helm/coredns-crd
          echo "${{ secrets.GITHUB_TOKEN }}" | helm registry login ghcr.io \
            --username ${{ github.actor }} --password-stdin
          helm push coredns-crd-${tag}.tgz oci://ghcr.io/${{ github.repository }}/charts

      - name: Sign chart
        run: |
          set -euo pipefail
          tag=${GITHUB_REF_NAME#v}
          cosign sign --yes \
            ghcr.io/${{ github.repository }}/charts/coredns-crd:${tag}

      - uses: softprops/action-gh-release@v2
        with:
          generate_release_notes: true
```

- [ ] **Step 8.2: Validate with actionlint**

```
~/go/bin/actionlint .github/workflows/release.yml
```
Expected: zero output.

- [ ] **Step 8.3: Commit**

```
git add .github/workflows/release.yml
git commit -m "ci: tag-driven release with cosign-signed image and chart"
```

---

## Task 9: E2E install on the k3s box

This task verifies the chart installs cleanly and provides parity with the
existing `config/`-based deploy. No commits.

- [ ] **Step 9.1: Sync the chart to the box**

```
rsync -azq --delete --exclude='.git' --exclude='/bin' --exclude='/dist' --exclude='.claude' \
  /Users/hbinhng/Documents/repos/personal/coredns-crd/ \
  hbinhng@192.168.1.34:~/coredns-crd/
```

- [ ] **Step 9.2: Uninstall the existing raw deployment**

```
ssh hbinhng@192.168.1.34 'kubectl -n kube-system delete -f ~/coredns-crd/deploy/coredns-deployment.yaml || true
kubectl delete -f ~/coredns-crd/config/rbac/ || true'
```

The CRD remains (it's not part of the raw RBAC manifests).

- [ ] **Step 9.3: Install via Helm**

```
ssh hbinhng@192.168.1.34 'helm install coredns-crd ~/coredns-crd/deploy/helm/coredns-crd \
  --namespace kube-system \
  --set image.repository=coredns-crd \
  --set image.tag=dev \
  --set image.pullPolicy=IfNotPresent \
  --set service.clusterIP=10.43.0.10 \
  --set fullnameOverride=kube-dns'
```

`fullnameOverride=kube-dns` keeps the Service named `kube-dns` so kubelet's
`--cluster-dns=10.43.0.10` resolves correctly.

- [ ] **Step 9.4: Verify rollout**

```
ssh hbinhng@192.168.1.34 'kubectl -n kube-system rollout status deployment/kube-dns --timeout=180s
kubectl -n kube-system get pods -l k8s-app=kube-dns
kubectl -n kube-system get lease coredns-crd-leader -o jsonpath="{.spec.holderIdentity}{\"\n\"}"'
```

Expected: deployment Ready with 2/2 replicas; lease holder identified.

- [ ] **Step 9.5: Verify resolution and metrics**

```
ssh hbinhng@192.168.1.34 'kubectl apply -f ~/coredns-crd/config/example/dnsslice.yaml
sleep 5
kubectl run digtest --rm -i --restart=Never --image=docker.io/alpine:3.20 --command -- \
  sh -c "apk add --no-cache bind-tools >/dev/null 2>&1 && dig +short @10.43.0.10 web.example.com A" 2>/dev/null | grep -E "^[0-9]"
LEADER_IP=$(kubectl get pods -n kube-system -l k8s-app=kube-dns -o jsonpath="{.items[0].status.podIP}")
kubectl run cm$RANDOM --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 --command -- \
  curl -s http://$LEADER_IP:9153/metrics 2>/dev/null | grep "^coredns_crd_" | head -10'
```

Expected: at least one A record returned and at least 8 `coredns_crd_*`
series visible.

- [ ] **Step 9.6: Cleanup (optional)**

If returning to raw-manifest mode:
```
ssh hbinhng@192.168.1.34 'helm uninstall coredns-crd -n kube-system
kubectl apply -f ~/coredns-crd/config/rbac/ -f ~/coredns-crd/deploy/coredns-deployment.yaml'
```

---

## Self-review

**Spec coverage:**
- Helm chart layout → Tasks 1–6 ✓
- Values shape → Task 1 ✓
- Templated Corefile → Task 4 ✓
- PDB + topology → Task 5 ✓
- crds/ install-once → Task 2 ✓
- CI workflow → Task 7 ✓
- Release workflow → Task 8 ✓
- Tag scheme + image labels + cosign keyless → Task 8 ✓
- E2E install → Task 9 ✓
- Verification command in README → Task 6 ✓

**Placeholder scan:** every step has runnable code. No "TBD" / "as
appropriate".

**Type consistency:**
- `coredns-crd.fullname`, `coredns-crd.selectorLabels`,
  `coredns-crd.serviceAccountName`, `coredns-crd.image`,
  `coredns-crd.leaseNamespace` — all defined in Task 2's `_helpers.tpl`,
  consumed identically in Tasks 3/4/5/6.
- Image tag: `.Values.image.tag` defaults to `.Chart.AppVersion` via
  `coredns-crd.image` helper; Task 8 sets `appVersion` to `${GITHUB_REF_NAME}`
  (e.g. `v0.1.0`), keeping the helper output stable.
- `fullnameOverride: kube-dns` documented in Task 6's README and used in
  Task 9's e2e step.

**Outstanding gap:** none.
