# NetworkPolicy + RBAC Tightening

**Sub-project D** in the production-readiness push.
**Date**: 2026-04-30.
**Status**: Approved (autonomous run; user delegated supervision).

## Problem

The production-readiness assessment flagged two hardening gaps:

1. **No network restriction.** A `coredns-crd` pod can talk to anything;
   anything can talk to it on any port. Cluster DNS is a high-value
   target — a compromised neighbor pod could silently amplify a DNS
   poisoning attack against any other workload.

2. **Coarse RBAC.** The single ClusterRole grants list/watch on every
   resource the kubernetes plugin might need (`endpoints`, `services`,
   `pods`, `namespaces`, `endpointslices`) plus events plus dnsslices.
   `endpoints` is deprecated in favor of EndpointSlice (GA since k8s
   1.21; our chart requires 1.28+) and `pods` is only needed when the
   kubernetes plugin's `pods` directive is enabled. Both are
   over-permissive today.

PodDisruptionBudget shipped with sub-project C's Helm chart.
NetworkPolicy and RBAC tightening are what's left.

## Goal

Ship the chart with opt-in NetworkPolicy templates and a ClusterRole
that grants exactly the permissions the configured Corefile actually
needs.

## Non-goals

- CNI-specific resources (Cilium ClusterwideNetworkPolicy, Calico
  GlobalNetworkPolicy). Vanilla `networking.k8s.io/v1.NetworkPolicy` is
  the common denominator.
- Egress restrictions. kube-apiserver IPs and DNS upstreams vary per
  cluster; restricting egress in a chart shipped across hundreds of
  cluster shapes is fragile. Operators layer their own egress NP on
  top.
- Pod Security Standards enforcement labels. Orthogonal — operators
  apply those at the namespace level.
- Splitting the ClusterRole into per-plugin ClusterRoles. The existing
  Helm conditional already gates the kubernetes-plugin rules; further
  decomposition is ceremony without payoff.

## Design

### NetworkPolicy

A single `templates/networkpolicy.yaml` gated by `networkPolicy.enabled`,
default `false`. Opt-in because:

- Many CNIs don't enforce NetworkPolicy. k3s's default flannel-with-NP
  controller does, but vanilla flannel does not. Calico and Cilium
  enforce by default.
- Operators tend to want explicit control over network rules and won't
  appreciate a chart silently restricting their cluster DNS.

Template:

```yaml
{{- if .Values.networkPolicy.enabled }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "coredns-crd.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "coredns-crd.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels: {{- include "coredns-crd.selectorLabels" . | nindent 6 }}
  policyTypes: [Ingress]
  ingress:
    # DNS from any pod in any namespace
    - from:
        - namespaceSelector: {}
      ports:
        - port: {{ .Values.service.ports.dns }}
          protocol: UDP
        - port: {{ .Values.service.ports.dnsTcp }}
          protocol: TCP
    {{- with .Values.networkPolicy.metricsAllowedFrom }}
    # Metrics scrape (configurable selector)
    - from: {{- toYaml . | nindent 8 }}
      ports:
        - port: {{ $.Values.service.ports.metrics }}
          protocol: TCP
    {{- end }}
{{- end }}
```

Values:

```yaml
networkPolicy:
  enabled: false
  # Ingress sources allowed to scrape the metrics port. Empty list = no
  # metrics ingress (Prometheus would be blocked). Default targets
  # kube-prometheus-stack's conventional layout.
  metricsAllowedFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
      podSelector:
        matchLabels:
          app.kubernetes.io/name: prometheus
```

**Egress is intentionally unrestricted.** Egress targets:
- kube-apiserver — IP varies (HA, certificate rotation).
- DNS upstreams configured in the Corefile `forward` directive.

These differ per cluster too much for a chart default to be safe.

**Probe traffic from kubelet (8080/8181) is not in the policy.** Kubelet
runs on the node's host network; most CNIs let host-network traffic
bypass NetworkPolicy entirely. If a CNI blocks it, the probe failure
surfaces immediately during `helm install` and the operator adds a
node-CIDR ingress rule. Documented in the README as a known surface.

### ClusterRole tightening

Two changes to `templates/clusterrole.yaml`:

1. **Drop `endpoints`.** EndpointSlice is GA since k8s 1.21; the chart
   requires 1.28+ via `kubeVersion`. The kubernetes plugin in CoreDNS
   1.12+ uses EndpointSlice exclusively when present.

2. **Gate `pods` on the `pods` directive being enabled.** The CoreDNS
   kubernetes plugin's `pods` directive has three modes:
   - `disabled` — no pod IP reverse resolution; no `pods` LIST/watch
     needed.
   - `insecure` — pod IP reverse resolution trusted from upstream;
     LIST/watch needed.
   - `verified` — same plus per-query verification; LIST/watch needed.

   Only `disabled` removes the requirement. Existing `corefile.kubernetes.pods` value is the gate.

Resulting rules block:

```yaml
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
    resources: ["services", "namespaces"]
    verbs: ["list", "watch"]
  {{- if ne .Values.corefile.kubernetes.pods "disabled" }}
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list", "watch"]
  {{- end }}
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["list", "watch"]
  {{- end }}
  # Conflict transition Events
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```

### README addendum

Add a "Hardening" section to `deploy/helm/coredns-crd/README.md`:

```markdown
## Hardening

### NetworkPolicy

`networkPolicy.enabled: true` adds a `NetworkPolicy` that allows ingress
to port 53 from any pod and to port 9153 from a configurable selector
(default: `app.kubernetes.io/name: prometheus` in the `monitoring`
namespace). Egress is unrestricted by design — kube-apiserver and DNS
upstream IPs vary per cluster.

NetworkPolicy is enforced only by CNIs that implement it: Calico,
Cilium, k3s's flannel+NP controller, and others. **Vanilla flannel does
not enforce.** Verify your CNI before relying on this.

Probe traffic (kubelet → ports 8080/8181) is allowed implicitly via
host-network bypass on most CNIs. If your CNI blocks it, add an
explicit ingress rule for your node CIDR.

### RBAC

The chart grants only the permissions the configured Corefile actually
needs:

- `corefile.kubernetes.enabled: false` removes services / namespaces /
  endpointslices / pods entirely.
- `corefile.kubernetes.pods: disabled` removes the pods rule even when
  the kubernetes plugin is enabled (no pod IP reverse resolution).
- `endpoints` is never granted (deprecated; chart requires k8s 1.28+
  where EndpointSlice is the only consumer).
```

### Failure modes

| Mode | Behavior |
|-|-|
| NetworkPolicy enabled on a non-enforcing CNI | NP is a no-op; documented in README. |
| Probe blocked by NP on a strict CNI | Pod fails liveness/readiness; operator adds node-CIDR rule. |
| Prometheus in a non-default namespace/pod-name | Scrape blocked; operator overrides `networkPolicy.metricsAllowedFrom`. |
| `corefile.kubernetes.pods: disabled` but operator enables `pods insecure` later | RBAC missing pods rule; CoreDNS logs reflector errors. Operator runs `helm upgrade` to re-grant. |
| Old cluster with no EndpointSlice | Out of scope; chart requires 1.28+. |

### Testing

- `helm lint deploy/helm/coredns-crd` — passes.
- `helm template ... --set networkPolicy.enabled=true | kubeconform
  -strict` — NP validates.
- `helm template ... --set corefile.kubernetes.pods=disabled` — rendered
  ClusterRole has no `pods` rule.
- `helm template ... | grep '"endpoints"'` — empty (negative
  assertion).
- E2E on k3s: enable NP, verify DNS resolution from an arbitrary pod
  still works and metrics scrape still succeeds.

## Acceptance criteria

1. `helm template ... --set networkPolicy.enabled=true ... |
   kubeconform -strict` passes.
2. With NP enabled on the k3s box, DNS resolution from a pod in any
   namespace still works and metrics-scrape from a `monitoring`-NS pod
   with `app.kubernetes.io/name: prometheus` succeeds.
3. Rendered ClusterRole with `corefile.kubernetes.pods=disabled` has no
   `pods` rule.
4. Rendered ClusterRole never contains `endpoints` (only
   `endpointslices`).
5. `helm lint` and CI gates remain green.
