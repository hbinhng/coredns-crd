# NetworkPolicy + RBAC Tightening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in NetworkPolicy template to the chart and tighten the ClusterRole so it grants only the permissions the configured Corefile actually needs.

**Architecture:** Single new template `templates/networkpolicy.yaml` gated on `networkPolicy.enabled` (default false). `templates/clusterrole.yaml` updated to drop the deprecated `endpoints` resource and to gate the `pods` rule on `corefile.kubernetes.pods != "disabled"`. README gains a Hardening section documenting both.

**Tech Stack:** Helm v3 templates, `networking.k8s.io/v1.NetworkPolicy`, `helm template | kubeconform` as the test harness.

---

## File structure

| File | Action | Responsibility |
|-|-|-|
| `deploy/helm/coredns-crd/values.yaml` | modify | Add `networkPolicy.enabled` + `networkPolicy.metricsAllowedFrom` defaults. |
| `deploy/helm/coredns-crd/templates/networkpolicy.yaml` | new | Renders a NetworkPolicy when enabled; allow-all-pods on port 53, configurable selector on metrics port. |
| `deploy/helm/coredns-crd/templates/clusterrole.yaml` | modify | Drop `endpoints`; gate `pods` on `corefile.kubernetes.pods != "disabled"`. |
| `deploy/helm/coredns-crd/README.md` | modify | Add a "Hardening" section documenting NetworkPolicy CNI requirements and the RBAC contract. |

---

## Task 1: Add `networkPolicy` values

**Files:**
- Modify: `deploy/helm/coredns-crd/values.yaml`

- [ ] **Step 1.1: Add the values block**

In `deploy/helm/coredns-crd/values.yaml`, append after the existing
`topologySpreadConstraints` section (or wherever convenient near the
end):

```yaml
networkPolicy:
  # NetworkPolicy is enforced only by CNIs that implement it (Calico,
  # Cilium, k3s with --flannel-backend=host-gw + the network-policy
  # controller). Vanilla flannel does NOT enforce. Opt in deliberately.
  enabled: false
  # Ingress sources allowed to scrape the metrics port. Empty list = no
  # metrics ingress (Prometheus would be blocked). Default targets the
  # kube-prometheus-stack convention: prometheus pods in `monitoring`.
  metricsAllowedFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
      podSelector:
        matchLabels:
          app.kubernetes.io/name: prometheus
```

- [ ] **Step 1.2: Verify lint still passes**

Run: `helm lint deploy/helm/coredns-crd`
Expected: `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 1.3: Commit**

```
git add deploy/helm/coredns-crd/values.yaml
git commit -m "feat(helm): add networkPolicy values (default disabled)"
```

---

## Task 2: Render the NetworkPolicy template (TDD via render assertion)

**Files:**
- Create: `deploy/helm/coredns-crd/templates/networkpolicy.yaml`

- [ ] **Step 2.1: Verify failure — NP not rendered today**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd --namespace kube-system --set networkPolicy.enabled=true | grep -c '^kind: NetworkPolicy'
```
Expected: `0` (template doesn't exist yet).

- [ ] **Step 2.2: Write the NetworkPolicy template**

Create `deploy/helm/coredns-crd/templates/networkpolicy.yaml`:

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

- [ ] **Step 2.3: Verify rendering when enabled**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  --set networkPolicy.enabled=true \
  | grep -c '^kind: NetworkPolicy'
```
Expected: `1`.

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  --set networkPolicy.enabled=true \
  | sed -n '/kind: NetworkPolicy/,/^---/p' \
  | grep -E '(port: 53|port: 9153)'
```
Expected: lines for both `port: 53` and `port: 9153`.

- [ ] **Step 2.4: Verify NP is hidden when disabled**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  | grep -c '^kind: NetworkPolicy'
```
Expected: `0` (default `networkPolicy.enabled: false`).

- [ ] **Step 2.5: Validate against the live API schema**

```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  --set networkPolicy.enabled=true \
  > /tmp/rendered.yaml
ssh hbinhng@192.168.1.34 'kubectl apply --dry-run=client -f -' < /tmp/rendered.yaml \
  | grep -i networkpolicy
```
Expected: `networkpolicy.networking.k8s.io/<name> created (dry run)` (no schema errors).

- [ ] **Step 2.6: Verify metricsAllowedFrom: [] hides the metrics rule**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.metricsAllowedFrom=null' \
  | sed -n '/kind: NetworkPolicy/,/^---/p' \
  | grep -c '9153'
```
Expected: `0` (no metrics rule when allowlist is empty).

- [ ] **Step 2.7: Commit**

```
git add deploy/helm/coredns-crd/templates/networkpolicy.yaml
git commit -m "feat(helm): opt-in NetworkPolicy template"
```

---

## Task 3: Tighten ClusterRole (drop endpoints, gate pods)

**Files:**
- Modify: `deploy/helm/coredns-crd/templates/clusterrole.yaml`

- [ ] **Step 3.1: Verify pre-state — endpoints present today**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd --namespace kube-system | grep -c '"endpoints"'
```
Expected: `1` (the existing rule).

- [ ] **Step 3.2: Replace the kubernetes-plugin block**

Open `deploy/helm/coredns-crd/templates/clusterrole.yaml` and replace
the rules block. The full file becomes:

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
  # kubernetes plugin: services + namespaces always; pods only when not "disabled".
  # Endpoints intentionally omitted — chart requires k8s 1.28+ (EndpointSlice GA since 1.21).
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

- [ ] **Step 3.3: Verify endpoints is gone**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd --namespace kube-system | grep -c '"endpoints"'
```
Expected: `0`.

- [ ] **Step 3.4: Verify pods rule disappears with `pods=disabled`**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set corefile.kubernetes.pods=disabled \
  | sed -n '/kind: ClusterRole/,/^---/p' \
  | grep -c '"pods"'
```
Expected: `0`.

- [ ] **Step 3.5: Verify pods rule still present with `pods=insecure` (default)**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  | sed -n '/kind: ClusterRole/,/^---/p' \
  | grep -c '"pods"'
```
Expected: `1`.

- [ ] **Step 3.6: Verify with `corefile.kubernetes.enabled=false` everything collapses to crd + events**

Run:
```
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set corefile.kubernetes.enabled=false \
  | sed -n '/kind: ClusterRole/,/^---/p' \
  | grep -E 'apiGroups:|resources:'
```
Expected: only the dnsslices and events lines (no services/namespaces/pods/endpointslices).

- [ ] **Step 3.7: Lint + render-validate**

Run:
```
helm lint deploy/helm/coredns-crd
helm template coredns-crd deploy/helm/coredns-crd \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10 \
  --set networkPolicy.enabled=true \
  > /tmp/rendered.yaml
ssh hbinhng@192.168.1.34 'kubectl apply --dry-run=client -f -' < /tmp/rendered.yaml \
  | tail -3
```
Expected: lint passes; dry-run reports `created (dry run)` for every resource without schema errors.

- [ ] **Step 3.8: Commit**

```
git add deploy/helm/coredns-crd/templates/clusterrole.yaml
git commit -m "refactor(helm): tighten ClusterRole — drop endpoints, gate pods"
```

---

## Task 4: README Hardening section

**Files:**
- Modify: `deploy/helm/coredns-crd/README.md`

- [ ] **Step 4.1: Append the Hardening section**

In `deploy/helm/coredns-crd/README.md`, append after the existing
"Values" table:

```markdown

## Hardening

### NetworkPolicy

`networkPolicy.enabled: true` adds a `NetworkPolicy` that allows ingress
to port 53 from any pod and to port 9153 from a configurable selector
(default: `app.kubernetes.io/name: prometheus` in the `monitoring`
namespace). Egress is unrestricted by design — kube-apiserver and DNS
upstream IPs vary per cluster.

NetworkPolicy is enforced only by CNIs that implement it: Calico,
Cilium, k3s's flannel-with-network-policy controller, and others.
**Vanilla flannel does not enforce.** Verify your CNI before relying on
this.

Probe traffic (kubelet → ports 8080/8181) is allowed implicitly via
host-network bypass on most CNIs. If your CNI blocks it, add an
explicit ingress rule for your node CIDR.

### RBAC

The chart grants only the permissions the configured Corefile actually
needs:

- `corefile.kubernetes.enabled: false` removes services, namespaces,
  endpointslices and pods entirely.
- `corefile.kubernetes.pods: disabled` removes the pods rule even when
  the kubernetes plugin is enabled (no pod IP reverse resolution).
- `endpoints` is never granted — the chart requires k8s 1.28+ where
  EndpointSlice is the only consumer for the kubernetes plugin.
```

- [ ] **Step 4.2: Commit**

```
git add deploy/helm/coredns-crd/README.md
git commit -m "docs(helm): Hardening section on NetworkPolicy + RBAC contract"
```

---

## Task 5: E2E on the k3s box

This task is verification-only; no commits.

- [ ] **Step 5.1: Sync to remote**

```
rsync -azq --delete --exclude='.git' --exclude='/bin' --exclude='/dist' --exclude='.claude' \
  /Users/hbinhng/Documents/repos/personal/coredns-crd/ \
  hbinhng@192.168.1.34:~/coredns-crd/
```

- [ ] **Step 5.2: helm upgrade with NetworkPolicy enabled**

```
ssh hbinhng@192.168.1.34 'helm upgrade coredns-crd ~/coredns-crd/deploy/helm/coredns-crd \
  --namespace kube-system \
  --set image.repository=coredns-crd \
  --set image.tag=dev \
  --set image.pullPolicy=IfNotPresent \
  --set service.clusterIP=10.43.0.10 \
  --set fullnameOverride=kube-dns \
  --set networkPolicy.enabled=true \
  | tail -10'
```

Expected: `STATUS: deployed`.

- [ ] **Step 5.3: Verify NetworkPolicy exists**

```
ssh hbinhng@192.168.1.34 'kubectl -n kube-system get networkpolicy
kubectl -n kube-system describe networkpolicy kube-dns | head -25'
```

Expected: NetworkPolicy `kube-dns` present with two ingress rules.

- [ ] **Step 5.4: Verify DNS resolution from a pod still works**

```
ssh hbinhng@192.168.1.34 'kubectl run digtest --rm -i --restart=Never --image=docker.io/alpine:3.20 --command -- \
  sh -c "apk add --no-cache bind-tools >/dev/null 2>&1 && dig +short @10.43.0.10 web.example.com A" 2>/dev/null \
  | grep -E "^[0-9]"'
```

Expected: at least one IPv4 returned.

- [ ] **Step 5.5: Verify metrics scrape still works (kube-prometheus-stack-style label)**

Use a fake Prometheus pod with the right labels in a `monitoring` namespace:

```
ssh hbinhng@192.168.1.34 'kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace monitoring kubernetes.io/metadata.name=monitoring --overwrite

LEADER_IP=$(kubectl get pods -n kube-system -l app.kubernetes.io/name=coredns-crd -o jsonpath="{.items[0].status.podIP}")

kubectl run prom-scrape --rm -i --restart=Never \
  --namespace monitoring \
  --labels app.kubernetes.io/name=prometheus \
  --image=docker.io/curlimages/curl:8.10.1 --command -- \
  curl -s -m 5 http://$LEADER_IP:9153/metrics 2>/dev/null \
  | grep "^coredns_crd_" | head -5'
```

Expected: at least 5 `coredns_crd_*` series visible.

- [ ] **Step 5.6: Verify metrics scrape from non-allowlisted source is blocked**

```
ssh hbinhng@192.168.1.34 'LEADER_IP=$(kubectl get pods -n kube-system -l app.kubernetes.io/name=coredns-crd -o jsonpath="{.items[0].status.podIP}")
# default namespace, no prometheus label — should be blocked by NP
kubectl run badscrape --rm -i --restart=Never --image=docker.io/curlimages/curl:8.10.1 --command -- \
  curl -s -m 5 -o /dev/null -w "%{http_code}\n" http://$LEADER_IP:9153/metrics 2>&1 \
  | tail -3'
```

Expected: connection times out (curl reports `000` or similar after 5 seconds), confirming the policy blocked the scrape. **Note**: this assumes k3s's flannel-with-NP controller is enforcing. If the test pod still gets a 200, the CNI isn't enforcing and the operational guarantee doesn't hold — which is the documented limitation.

- [ ] **Step 5.7: Cleanup if needed**

```
ssh hbinhng@192.168.1.34 'kubectl delete namespace monitoring 2>/dev/null || true'
```

---

## Self-review

**Spec coverage:**
- NetworkPolicy template + opt-in default → Tasks 1, 2 ✓
- ClusterRole drops endpoints → Task 3 ✓
- ClusterRole gates pods on `corefile.kubernetes.pods` → Task 3 ✓
- README Hardening section → Task 4 ✓
- Acceptance #1 (kubeconform pass) → Task 2.5, Task 3.7 ✓
- Acceptance #2 (DNS + scrape work post-NP) → Tasks 5.4, 5.5 ✓
- Acceptance #3 (no pods rule with `pods=disabled`) → Task 3.4 ✓
- Acceptance #4 (no endpoints) → Task 3.3 ✓
- Acceptance #5 (lint + CI gates green) → Task 2.5, Task 3.7 ✓

**Placeholder scan:** every code step has runnable code; every command
has expected output.

**Type consistency:**
- Values key `networkPolicy.enabled` consumed identically in Tasks 1
  (default) and 2 (gate).
- `networkPolicy.metricsAllowedFrom` defaults a YAML list in Task 1; the
  template in Task 2 uses `with .Values.networkPolicy.metricsAllowedFrom`
  which correctly hides the rule when nil/empty.
- `corefile.kubernetes.pods` referenced as a string in Task 3's
  `ne ... "disabled"`; existing `values.yaml` already declares it as
  `pods: insecure`.

**Outstanding gap:** none.
