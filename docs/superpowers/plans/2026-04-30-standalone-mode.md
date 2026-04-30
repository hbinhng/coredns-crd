# Standalone DNS server mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `v0.2.0` of coredns-crd with a `values-standalone.yaml` overlay that lets users run the chart side-by-side with the cluster's existing CoreDNS as a declarative authoritative DNS server.

**Architecture:** Pure Helm chart change (no Go code). The existing `crd` plugin is already zone-agnostic. We make `kubernetes` and `forward` blocks conditional in the Corefile, add a LoadBalancer Service template, gate hostNetwork on the Deployment, ship a public-LB recursion guard, and add a `values-standalone.yaml` overlay that flips the relevant defaults. All other chart concerns (RBAC, leader-election, PDB, NetworkPolicy, image, ServiceAccount) are unchanged.

**Tech Stack:** Helm 3, helm-unittest, CoreDNS plugin SDK (Go — read-only here, no changes), kubectl, KinD (e2e), bash. Independent agentic verification via the Agent tool with `code-reviewer` subagent.

**Spec:** `docs/superpowers/specs/2026-04-30-standalone-mode-design.md`

**Coverage bar:** No new Go code is required, so the 100% coverage bar applies to ensuring existing Go test coverage is **not regressed**. The plan's verification step in Task 12 includes a `go test -cover ./...` regression check.

---

## File map

**New files:**
- `deploy/helm/coredns-crd/values-standalone.yaml` — overlay file for standalone installs
- `deploy/helm/coredns-crd/templates/service-lb.yaml` — LoadBalancer Service template
- `deploy/helm/coredns-crd/tests/configmap_test.yaml` — Corefile rendering assertions
- `deploy/helm/coredns-crd/tests/service-lb_test.yaml` — LoadBalancer Service rendering
- `deploy/helm/coredns-crd/tests/deployment_test.yaml` — hostNetwork rendering
- `deploy/helm/coredns-crd/tests/recursion-guard_test.yaml` — public-LB guard behavior
- `deploy/helm/coredns-crd/tests/back-compat_test.yaml` — `corefile.forward.upstream` string back-compat

**Modified files:**
- `deploy/helm/coredns-crd/values.yaml` — add `service.loadBalancer`, `hostNetwork`, restructure `corefile.forward`
- `deploy/helm/coredns-crd/templates/_helpers.tpl` — new helpers for forward/kubernetes/recursion-guard
- `deploy/helm/coredns-crd/templates/configmap.yaml` — wrap `kubernetes` + `forward` blocks in helpers
- `deploy/helm/coredns-crd/templates/deployment.yaml` — gate `hostNetwork` + `dnsPolicy` on values
- `deploy/helm/coredns-crd/templates/NOTES.txt` — add ingress recipe pointer
- `deploy/helm/coredns-crd/Chart.yaml` — bump `version` to `0.2.0`
- `test/e2e/run.sh` — add Scenario 5 (standalone install side-by-side)
- `.github/workflows/ci.yml` — add helm-unittest step
- `README.md` — new "Deployment modes" section
- `CHANGELOG.md` — v0.2.0 unreleased entry

---

## Task 1: helm-unittest setup + regression baseline

Goal: bring helm-unittest into the project as the chart-template test framework, and lock in a regression baseline that asserts `helm template` with default values produces *exactly* the same Corefile and Service shapes as v0.1.0. Every subsequent task adds tests; this one provides the safety net so we don't drift the default install.

**Files:**
- Modify: `.github/workflows/ci.yml` — install helm-unittest, run it
- Create: `deploy/helm/coredns-crd/tests/configmap_test.yaml`
- Create: `deploy/helm/coredns-crd/tests/service_test.yaml`

- [ ] **Step 1: Read current CI workflow**

Run: `cat .github/workflows/ci.yml`
Note the structure (job names, steps) so the new step lands cleanly.

- [ ] **Step 2: Add helm-unittest install + run step to CI**

Edit `.github/workflows/ci.yml`. After the `go test` step in the `test` job, add:

```yaml
      - uses: azure/setup-helm@v4
        with:
          version: v3.15.4
      - name: Install helm-unittest plugin
        run: helm plugin install https://github.com/helm-unittest/helm-unittest --version v0.6.2
      - name: Run helm-unittest
        run: helm unittest deploy/helm/coredns-crd
```

Pin `v0.6.2` (matches Helm 3.15 compat). Pin the action version to match `release.yml`'s existing `azure/setup-helm@v4` for consistency.

- [ ] **Step 3: Write the failing baseline test for the default Corefile**

Create `deploy/helm/coredns-crd/tests/configmap_test.yaml`:

```yaml
suite: configmap (Corefile)
templates:
  - configmap.yaml

tests:
  - it: default values render the cluster-DNS Corefile (regression baseline)
    asserts:
      - equal:
          path: data.Corefile
          value: |2

            .:53 {
                errors
                health {
                  lameduck 5s
                }
                ready
                crd {
                  fallthrough
                }
                kubernetes cluster.local in-addr.arpa ip6.arpa {
                  pods insecure
                  fallthrough in-addr.arpa ip6.arpa
                  ttl 30
                }
                prometheus :9153
                forward . /etc/resolv.conf {
                  max_concurrent 1000
                }
                cache 30
                loop
                reload
                loadbalance
            }
```

Note the leading `|2` and blank first line — `helm template` emits a leading newline because of the `range` block in `configmap.yaml`. Verify by hand-running the next step before locking in the indentation.

- [ ] **Step 4: Run the test against the *current* (unchanged) chart**

Run:
```bash
helm plugin install https://github.com/helm-unittest/helm-unittest --version v0.6.2 || true
helm unittest deploy/helm/coredns-crd -f tests/configmap_test.yaml
```

Expected: PASS (this is a baseline, not red-then-green; it should match current rendering immediately). If it fails on whitespace, run `helm template deploy/helm/coredns-crd | grep -A40 'Corefile: |'` and copy the *literal* rendered Corefile into the test fixture. The `value:` field must match byte-for-byte, including trailing newlines.

- [ ] **Step 5: Write the baseline Service test**

Create `deploy/helm/coredns-crd/tests/service_test.yaml`:

```yaml
suite: service (ClusterIP)
templates:
  - service.yaml

tests:
  - it: default values render a ClusterIP Service with three named ports
    asserts:
      - equal: { path: spec.type, value: ClusterIP }
      - equal: { path: spec.ports[0].name, value: dns }
      - equal: { path: spec.ports[0].port, value: 53 }
      - equal: { path: spec.ports[0].protocol, value: UDP }
      - equal: { path: spec.ports[1].name, value: dns-tcp }
      - equal: { path: spec.ports[1].port, value: 53 }
      - equal: { path: spec.ports[1].protocol, value: TCP }
      - equal: { path: spec.ports[2].name, value: metrics }
      - equal: { path: spec.ports[2].port, value: 9153 }
      - equal: { path: spec.ports[2].protocol, value: TCP }

  - it: clusterIP value is propagated when set
    set:
      service.clusterIP: 10.96.0.10
    asserts:
      - equal: { path: spec.clusterIP, value: 10.96.0.10 }
```

- [ ] **Step 6: Run all tests and verify pass**

Run:
```bash
helm unittest deploy/helm/coredns-crd
```

Expected: 2 PASS (1 in configmap, 1 in service with multiple asserts). If the configmap baseline fails on whitespace pedantry, fix the fixture. Do NOT change the template to make the test pass — this is a regression baseline.

- [ ] **Step 7: Commit**

```bash
git add .github/workflows/ci.yml deploy/helm/coredns-crd/tests/
git commit -m "test(helm): add helm-unittest baseline for default install"
```

---

## Task 2: forward-block conditionalization + upstreams list

Goal: today the `forward .` block in the Corefile is unconditional and uses a single `corefile.forward.upstream` string. Make the block emit only when `corefile.forward.upstreams` (new list) is non-empty, with a back-compat helper that resolves the legacy `upstream` string into a list.

**Files:**
- Modify: `deploy/helm/coredns-crd/values.yaml`
- Modify: `deploy/helm/coredns-crd/templates/_helpers.tpl`
- Modify: `deploy/helm/coredns-crd/templates/configmap.yaml`
- Create: `deploy/helm/coredns-crd/tests/back-compat_test.yaml`
- Modify: `deploy/helm/coredns-crd/tests/configmap_test.yaml`

- [ ] **Step 1: Write failing test — empty upstreams suppresses the forward block**

Append to `deploy/helm/coredns-crd/tests/configmap_test.yaml`:

```yaml
  - it: empty corefile.forward.upstreams + empty upstream suppresses forward block
    set:
      corefile.forward.upstream: ""
      corefile.forward.upstreams: []
    asserts:
      - notMatchRegex:
          path: data.Corefile
          pattern: '(?m)^\s*forward\s+\.'

  - it: corefile.forward.upstreams renders multi-upstream forward block
    set:
      corefile.forward.upstream: ""
      corefile.forward.upstreams:
        - 1.1.1.1
        - 9.9.9.9
    asserts:
      - matchRegex:
          path: data.Corefile
          pattern: '(?ms)forward\s+\.\s+1\.1\.1\.1\s+9\.9\.9\.9\s*\{'
```

Note: empty string (`""`) is used instead of `null` because helm-unittest's
`set:` semantics around null vary by version. The helper treats both as falsey
via `if .Values.corefile.forward.upstream`, so `""` produces identical behavior.

- [ ] **Step 2: Write failing back-compat test**

Create `deploy/helm/coredns-crd/tests/back-compat_test.yaml`:

```yaml
suite: back-compat (legacy values shapes)
templates:
  - configmap.yaml

tests:
  - it: legacy corefile.forward.upstream string still works (resolves to single-element upstreams)
    set:
      corefile.forward.upstream: /etc/resolv.conf
      # Do NOT set corefile.forward.upstreams — back-compat path
    asserts:
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+forward\s+\.\s+/etc/resolv\.conf\s*\{'
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+max_concurrent\s+1000'

  - it: legacy upstream string is overridden by explicit upstreams list
    set:
      corefile.forward.upstream: /etc/resolv.conf
      corefile.forward.upstreams: [1.1.1.1]
    asserts:
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+forward\s+\.\s+1\.1\.1\.1\s*\{'
      - notMatchRegex:
          path: data.Corefile
          pattern: '/etc/resolv\.conf'
```

- [ ] **Step 3: Run tests to verify they fail**

Run:
```bash
helm unittest deploy/helm/coredns-crd
```

Expected: FAIL — configmap_test.yaml's "empty upstreams suppresses" fails (forward is unconditional today), "renders multi-upstream" fails (today's template uses singular `upstream`), back-compat tests fail (no `upstreams` schema yet).

- [ ] **Step 4: Add `upstreams` field to values.yaml (preserve `upstream` for back-compat)**

Edit `deploy/helm/coredns-crd/values.yaml`. Replace the `forward:` block:

```yaml
  forward:
    # Either set `upstreams` (list, preferred) OR `upstream` (string, legacy).
    # If `upstreams` is empty AND `upstream` is empty/null, the forward block
    # is omitted from the Corefile (authoritative-only behavior).
    upstreams: []
    upstream: /etc/resolv.conf  # legacy single-string form, kept for back-compat
    maxConcurrent: 1000
    # Public-LB recursion guard: must be true to combine upstreams with
    # service.loadBalancer.enabled. See README "Deployment modes."
    allowPublicRecursion: false
```

- [ ] **Step 5: Add `coredns-crd.forwardUpstreams` helper**

Append to `deploy/helm/coredns-crd/templates/_helpers.tpl`:

```yaml
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
```

- [ ] **Step 6: Wrap the forward block in configmap.yaml**

In `deploy/helm/coredns-crd/templates/configmap.yaml`, replace the existing forward stanza:

```yaml
        forward . {{ $.Values.corefile.forward.upstream }} {
          max_concurrent {{ $.Values.corefile.forward.maxConcurrent }}
        }
```

with:

```yaml
        {{- if include "coredns-crd.forwardEnabled" $ }}
        forward . {{ include "coredns-crd.forwardUpstreams" $ }} {
          max_concurrent {{ $.Values.corefile.forward.maxConcurrent }}
        }
        {{- end }}
```

- [ ] **Step 7: Run tests, verify pass**

Run:
```bash
helm unittest deploy/helm/coredns-crd
```

Expected: All four new tests + the original baseline PASS. The original baseline still asserts the forward block IS present with `/etc/resolv.conf` — which works because the legacy `upstream: /etc/resolv.conf` default in values.yaml is preserved.

- [ ] **Step 8: Commit**

```bash
git add deploy/helm/coredns-crd/values.yaml deploy/helm/coredns-crd/templates/_helpers.tpl deploy/helm/coredns-crd/templates/configmap.yaml deploy/helm/coredns-crd/tests/configmap_test.yaml deploy/helm/coredns-crd/tests/back-compat_test.yaml
git commit -m "feat(chart): make forward block conditional, add upstreams list"
```

---

## Task 3: kubernetes-block conditionalization

Goal: the `kubernetes` plugin block in `configmap.yaml` is already gated on `corefile.kubernetes.enabled`, but there's no test coverage proving the gate works. Add tests to lock the behavior, and refactor the gate behind a named helper for symmetry with the new `forwardEnabled` helper.

**Files:**
- Modify: `deploy/helm/coredns-crd/templates/_helpers.tpl`
- Modify: `deploy/helm/coredns-crd/templates/configmap.yaml`
- Modify: `deploy/helm/coredns-crd/tests/configmap_test.yaml`

- [ ] **Step 1: Write failing tests for kubernetes-block gating**

Append to `deploy/helm/coredns-crd/tests/configmap_test.yaml`:

```yaml
  - it: corefile.kubernetes.enabled=false suppresses the kubernetes block
    set:
      corefile.kubernetes.enabled: false
    asserts:
      - notMatchRegex:
          path: data.Corefile
          pattern: '(?m)^\s*kubernetes\s'

  - it: corefile.kubernetes.enabled=false still emits the crd plugin
    set:
      corefile.kubernetes.enabled: false
    asserts:
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+crd\s*\{'
```

- [ ] **Step 2: Run tests, verify both pass already**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: PASS — the existing template already gates on `corefile.kubernetes.enabled`. We're adding regression coverage, not behavior.

- [ ] **Step 3: Add `kubernetesEnabled` helper for symmetry**

Append to `deploy/helm/coredns-crd/templates/_helpers.tpl`:

```yaml
{{/*
  kubernetesEnabled: returns "true" iff the Corefile should include the
  kubernetes plugin block. Disabled in standalone installs (cluster's
  existing CoreDNS owns cluster.local).
*/}}
{{- define "coredns-crd.kubernetesEnabled" -}}
{{- if .Values.corefile.kubernetes.enabled -}}true{{- end -}}
{{- end -}}
```

- [ ] **Step 4: Refactor configmap.yaml to use the helper**

In `deploy/helm/coredns-crd/templates/configmap.yaml`, replace:

```yaml
        {{- if $.Values.corefile.kubernetes.enabled }}
```

with:

```yaml
        {{- if include "coredns-crd.kubernetesEnabled" $ }}
```

- [ ] **Step 5: Run tests, verify still passing**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: PASS — refactor is behavior-preserving.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/coredns-crd/templates/_helpers.tpl deploy/helm/coredns-crd/templates/configmap.yaml deploy/helm/coredns-crd/tests/configmap_test.yaml
git commit -m "refactor(chart): wrap kubernetes-block gate in named helper"
```

---

## Task 4: values-standalone.yaml overlay

Goal: ship the overlay file users pass to `helm install -f` to flip the chart into standalone behavior. Test that the overlay produces a Corefile *without* `kubernetes` and *without* `forward`.

**Files:**
- Create: `deploy/helm/coredns-crd/values-standalone.yaml`
- Modify: `deploy/helm/coredns-crd/tests/configmap_test.yaml`

- [ ] **Step 1: Write failing standalone-overlay test**

Append to `deploy/helm/coredns-crd/tests/configmap_test.yaml`:

```yaml
  - it: standalone overlay (-f values-standalone.yaml) renders without kubernetes and without forward
    values:
      - ../values-standalone.yaml
    asserts:
      - notMatchRegex:
          path: data.Corefile
          pattern: '(?m)^\s*kubernetes\s'
      - notMatchRegex:
          path: data.Corefile
          pattern: '(?m)^\s*forward\s+\.'
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+crd\s*\{'

  - it: standalone overlay + explicit upstreams renders forward block
    values:
      - ../values-standalone.yaml
    set:
      corefile.forward.upstreams: [1.1.1.1]
    asserts:
      - matchRegex:
          path: data.Corefile
          pattern: '(?m)^\s+forward\s+\.\s+1\.1\.1\.1\s*\{'
```

helm-unittest's `values:` field takes paths relative to the suite file's directory; the overlay sits one dir up.

- [ ] **Step 2: Run, verify FAIL (overlay file does not exist yet)**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: FAIL — "could not find values file ../values-standalone.yaml" or similar.

- [ ] **Step 3: Create the overlay file**

Create `deploy/helm/coredns-crd/values-standalone.yaml`:

```yaml
# Standalone deployment overlay for coredns-crd.
#
# Pass to `helm install -f deploy/helm/coredns-crd/values-standalone.yaml`
# to run coredns-crd side-by-side with the cluster's existing CoreDNS as
# a declarative authoritative DNS server, rather than as the cluster's
# tier-0 DNS replacement.
#
# See README "Deployment modes" for ingress recipes (stub-domain via
# main CoreDNS, per-pod dnsConfig, hostNetwork, LoadBalancer, NodePort).

corefile:
  kubernetes:
    # No `kubernetes` plugin block — the cluster's existing CoreDNS
    # owns cluster.local resolution.
    enabled: false
  forward:
    # Authoritative-only by default. Set this to a non-empty list to
    # enable recursion (e.g. [1.1.1.1, 9.9.9.9]). If you also enable
    # service.loadBalancer below, you MUST set allowPublicRecursion: true
    # to acknowledge the open-resolver risk.
    upstreams: []
    # Override the legacy back-compat default to match upstreams: [].
    upstream: ""
    allowPublicRecursion: false

# The chart's default Service is ClusterIP and has no fullnameOverride,
# so it does not collide with kube-dns. No further overrides needed for
# the basic in-cluster install. For out-of-cluster ingress, uncomment one
# of the blocks below (or pass a second -f my-overrides.yaml).

# service:
#   loadBalancer:
#     enabled: true
#     externalTrafficPolicy: Local
#     annotations:
#       networking.gke.io/load-balancer-type: Internal

# hostNetwork:
#   enabled: true
#   dnsPolicy: ClusterFirstWithHostNet
```

- [ ] **Step 4: Run tests, verify pass**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: PASS for all standalone-overlay tests. If "renders forward block with explicit upstreams" fails, double-check that the overlay sets `upstream: ""` (without that, the legacy back-compat helper resurrects it).

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/coredns-crd/values-standalone.yaml deploy/helm/coredns-crd/tests/configmap_test.yaml
git commit -m "feat(chart): add values-standalone.yaml overlay"
```

---

## Task 5: hostNetwork support

Goal: gate `hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet` on the Deployment behind a new `hostNetwork.enabled` value. This unlocks ingress path #3 (node IPs become DNS servers on :53 directly, no LB or NodePort).

**Files:**
- Modify: `deploy/helm/coredns-crd/values.yaml`
- Modify: `deploy/helm/coredns-crd/templates/deployment.yaml`
- Create: `deploy/helm/coredns-crd/tests/deployment_test.yaml`

- [ ] **Step 1: Write failing tests**

Create `deploy/helm/coredns-crd/tests/deployment_test.yaml`:

```yaml
suite: deployment
templates:
  - deployment.yaml

tests:
  - it: default — hostNetwork is unset and dnsPolicy is Default
    asserts:
      - notExists: { path: spec.template.spec.hostNetwork }
      - equal:
          path: spec.template.spec.dnsPolicy
          value: Default

  - it: hostNetwork.enabled=true — sets hostNetwork and dnsPolicy
    set:
      hostNetwork.enabled: true
    asserts:
      - equal: { path: spec.template.spec.hostNetwork, value: true }
      - equal:
          path: spec.template.spec.dnsPolicy
          value: ClusterFirstWithHostNet

  - it: hostNetwork.enabled=true with custom dnsPolicy override
    set:
      hostNetwork.enabled: true
      hostNetwork.dnsPolicy: None
    asserts:
      - equal: { path: spec.template.spec.hostNetwork, value: true }
      - equal: { path: spec.template.spec.dnsPolicy, value: None }
```

- [ ] **Step 2: Run, verify FAIL**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: FAIL — `hostNetwork` value path doesn't exist yet, and the template hardcodes `dnsPolicy: Default`.

- [ ] **Step 3: Add `hostNetwork` to values.yaml**

In `deploy/helm/coredns-crd/values.yaml`, after the `nodeSelector:` block, add:

```yaml
hostNetwork:
  # Run pods in the node's network namespace. Each node IP becomes a
  # DNS server on :53. Useful for homelab / bare-metal / "router uses
  # these IPs as upstream DNS." Requires that no other process on the
  # node binds :53. NetworkPolicy does not apply to hostNetwork pods.
  enabled: false
  # When hostNetwork is enabled, dnsPolicy must be set explicitly so the
  # pod's own DNS resolution still works. ClusterFirstWithHostNet keeps
  # cluster.local resolvable from inside the pod.
  dnsPolicy: ClusterFirstWithHostNet
```

- [ ] **Step 4: Wire `hostNetwork` into `deployment.yaml`**

In `deploy/helm/coredns-crd/templates/deployment.yaml`, find the line:

```yaml
      dnsPolicy: Default
```

Replace it with:

```yaml
      {{- if .Values.hostNetwork.enabled }}
      hostNetwork: true
      dnsPolicy: {{ .Values.hostNetwork.dnsPolicy }}
      {{- else }}
      dnsPolicy: Default
      {{- end }}
```

- [ ] **Step 5: Run tests, verify pass**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: PASS for all three deployment tests.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/coredns-crd/values.yaml deploy/helm/coredns-crd/templates/deployment.yaml deploy/helm/coredns-crd/tests/deployment_test.yaml
git commit -m "feat(chart): gate hostNetwork on hostNetwork.enabled value"
```

---

## Task 6: LoadBalancer Service template

Goal: ship a second Service (type LoadBalancer) gated on `service.loadBalancer.enabled`, with `externalTrafficPolicy: Local` as the default for source-IP preservation.

**Files:**
- Modify: `deploy/helm/coredns-crd/values.yaml`
- Create: `deploy/helm/coredns-crd/templates/service-lb.yaml`
- Create: `deploy/helm/coredns-crd/tests/service-lb_test.yaml`

- [ ] **Step 1: Write failing tests**

Create `deploy/helm/coredns-crd/tests/service-lb_test.yaml`:

```yaml
suite: service-lb (LoadBalancer Service)
templates:
  - service-lb.yaml

tests:
  - it: default — LoadBalancer Service is not rendered
    asserts:
      - hasDocuments: { count: 0 }

  - it: service.loadBalancer.enabled=true — renders LoadBalancer Service
    set:
      service.loadBalancer.enabled: true
    asserts:
      - hasDocuments: { count: 1 }
      - equal: { path: spec.type, value: LoadBalancer }
      - equal: { path: spec.externalTrafficPolicy, value: Local }
      - equal: { path: spec.ports[0].name, value: dns }
      - equal: { path: spec.ports[0].port, value: 53 }
      - equal: { path: spec.ports[0].protocol, value: UDP }
      - equal: { path: spec.ports[1].name, value: dns-tcp }
      - equal: { path: spec.ports[1].port, value: 53 }
      - equal: { path: spec.ports[1].protocol, value: TCP }
      - notExists: { path: spec.ports[2] }  # no metrics port on LB

  - it: loadBalancerClass override
    set:
      service.loadBalancer.enabled: true
      service.loadBalancer.loadBalancerClass: kube-vip.io/kube-vip-class
    asserts:
      - equal:
          path: spec.loadBalancerClass
          value: kube-vip.io/kube-vip-class

  - it: loadBalancerIP override
    set:
      service.loadBalancer.enabled: true
      service.loadBalancer.loadBalancerIP: 192.168.1.53
    asserts:
      - equal:
          path: spec.loadBalancerIP
          value: 192.168.1.53

  - it: annotations are propagated
    set:
      service.loadBalancer.enabled: true
      service.loadBalancer.annotations:
        networking.gke.io/load-balancer-type: Internal
    asserts:
      - equal:
          path: metadata.annotations["networking.gke.io/load-balancer-type"]
          value: Internal

  - it: externalTrafficPolicy override
    set:
      service.loadBalancer.enabled: true
      service.loadBalancer.externalTrafficPolicy: Cluster
    asserts:
      - equal: { path: spec.externalTrafficPolicy, value: Cluster }

  - it: name suffix distinguishes from ClusterIP service
    set:
      service.loadBalancer.enabled: true
    asserts:
      - matchRegex:
          path: metadata.name
          pattern: '-lb$'
```

- [ ] **Step 2: Run, verify FAIL**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: FAIL — `service-lb.yaml` doesn't exist; test framework errors on missing template file.

- [ ] **Step 3: Add `service.loadBalancer` to values.yaml**

In `deploy/helm/coredns-crd/values.yaml`, inside the existing `service:` block, after the `annotations:` field, add:

```yaml
  loadBalancer:
    # Render a second Service of type LoadBalancer pointing at the same
    # Deployment. Useful for exposing DNS to clients outside the cluster.
    # The metrics port is intentionally excluded from the LB Service.
    enabled: false
    # `Local` preserves the client source IP; `Cluster` SNATs through any
    # node. DNS access policies and audit logging want `Local`.
    externalTrafficPolicy: Local
    # Optional. Useful for MetalLB / kube-vip / Cilium L2 announcement.
    loadBalancerClass: ""
    # Optional. Most cloud providers ignore this; useful for bare-metal
    # LB controllers that honor it.
    loadBalancerIP: ""
    # Optional. Common values:
    #   networking.gke.io/load-balancer-type: Internal
    #   service.beta.kubernetes.io/aws-load-balancer-internal: "true"
    #   metallb.io/loadBalancerIPs: "192.168.1.53"
    annotations: {}
```

- [ ] **Step 4: Create `service-lb.yaml`**

Create `deploy/helm/coredns-crd/templates/service-lb.yaml`:

```yaml
{{- if .Values.service.loadBalancer.enabled -}}
{{- include "coredns-crd.publicLBRecursionGuard" . -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "coredns-crd.fullname" . }}-lb
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "coredns-crd.labels" . | nindent 4 }}
    coredns-crd.io/service-role: load-balancer
  {{- with .Values.service.loadBalancer.annotations }}
  annotations: {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: LoadBalancer
  externalTrafficPolicy: {{ .Values.service.loadBalancer.externalTrafficPolicy }}
  {{- with .Values.service.loadBalancer.loadBalancerClass }}
  loadBalancerClass: {{ . }}
  {{- end }}
  {{- with .Values.service.loadBalancer.loadBalancerIP }}
  loadBalancerIP: {{ . }}
  {{- end }}
  selector: {{- include "coredns-crd.selectorLabels" . | nindent 4 }}
  ports:
    - name: dns
      port: {{ .Values.service.ports.dns }}
      protocol: UDP
    - name: dns-tcp
      port: {{ .Values.service.ports.dnsTcp }}
      protocol: TCP
{{- end }}
```

The `coredns-crd.publicLBRecursionGuard` helper is added in the next task; the include here is a forward reference. helm-unittest will currently emit "could not find template ... publicLBRecursionGuard" — that's expected and resolves in Task 7.

- [ ] **Step 5: Run tests, expect partial pass**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: most asserts PASS, but the suite fails because the recursion-guard helper isn't defined yet. The test "default — LoadBalancer Service is not rendered" should still PASS (the `if` short-circuits before the include).

- [ ] **Step 6: Commit (knowing one helper is forward-declared)**

```bash
git add deploy/helm/coredns-crd/values.yaml deploy/helm/coredns-crd/templates/service-lb.yaml deploy/helm/coredns-crd/tests/service-lb_test.yaml
git commit -m "feat(chart): add LoadBalancer Service for out-of-cluster DNS"
```

The next task adds the `publicLBRecursionGuard` helper that this template forward-references; the tests will go fully green there.

---

## Task 7: public-LB recursion guard

Goal: when `service.loadBalancer.enabled: true` AND `corefile.forward.upstreams` (or legacy `upstream`) is non-empty AND `corefile.forward.allowPublicRecursion` is false, fail at template-render time. Prevents accidentally shipping an open recursive DNS resolver to the public internet.

**Files:**
- Modify: `deploy/helm/coredns-crd/templates/_helpers.tpl`
- Create: `deploy/helm/coredns-crd/tests/recursion-guard_test.yaml`

- [ ] **Step 1: Write failing tests**

Create `deploy/helm/coredns-crd/tests/recursion-guard_test.yaml`:

```yaml
suite: public-LB recursion guard
templates:
  - service-lb.yaml

tests:
  - it: LB enabled + no forward upstreams = no guard fire (OK)
    set:
      service.loadBalancer.enabled: true
      corefile.forward.upstream: ""
      corefile.forward.upstreams: []
    asserts:
      - hasDocuments: { count: 1 }
      - equal: { path: spec.type, value: LoadBalancer }

  - it: LB enabled + upstreams + allowPublicRecursion=true = no guard fire
    set:
      service.loadBalancer.enabled: true
      corefile.forward.upstreams: [1.1.1.1]
      corefile.forward.allowPublicRecursion: true
    asserts:
      - hasDocuments: { count: 1 }
      - equal: { path: spec.type, value: LoadBalancer }

  - it: LB enabled + legacy upstream + allowPublicRecursion=false = guard fires
    set:
      service.loadBalancer.enabled: true
      # Default values keep upstream=/etc/resolv.conf, allowPublicRecursion=false
    asserts:
      - failedTemplate:
          errorPattern: open recursive resolver

  - it: LB enabled + new upstreams + allowPublicRecursion=false = guard fires
    set:
      service.loadBalancer.enabled: true
      corefile.forward.upstreams: [1.1.1.1]
      corefile.forward.allowPublicRecursion: false
    asserts:
      - failedTemplate:
          errorPattern: open recursive resolver

  - it: LB disabled + upstreams + allowPublicRecursion=false = no guard fire
    set:
      service.loadBalancer.enabled: false
      corefile.forward.upstreams: [1.1.1.1]
    asserts:
      - hasDocuments: { count: 0 }
```

- [ ] **Step 2: Run, verify FAIL**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: helm-unittest reports template-render errors (helper undefined) on every test in the new suite, plus the "default — LoadBalancer Service is not rendered" test from Task 6 fails for the same reason.

- [ ] **Step 3: Add the guard helper**

Append to `deploy/helm/coredns-crd/templates/_helpers.tpl`:

```yaml
{{/*
  publicLBRecursionGuard: fails template rendering when:
    - service.loadBalancer.enabled is true, AND
    - the forward block would emit non-empty upstreams, AND
    - corefile.forward.allowPublicRecursion is false.
  Prevents accidentally shipping an open recursive DNS resolver to the
  public internet (DDoS amplification risk). Users who want recursion
  on a confirmed-internal LB must set allowPublicRecursion: true.
*/}}
{{- define "coredns-crd.publicLBRecursionGuard" -}}
{{- if and .Values.service.loadBalancer.enabled (include "coredns-crd.forwardEnabled" .) -}}
{{- if not .Values.corefile.forward.allowPublicRecursion -}}
{{- fail (printf "Refusing to render: enabling service.loadBalancer (LoadBalancer Service) together with corefile.forward.upstreams (recursive forwarding) would expose an open recursive resolver if the LB is internet-reachable. If this LB is internal-only (e.g. networking.gke.io/load-balancer-type: Internal, service.beta.kubernetes.io/aws-load-balancer-internal: \"true\", or a private-network MetalLB pool), set corefile.forward.allowPublicRecursion: true to confirm. Otherwise, remove corefile.forward.upstreams and corefile.forward.upstream.") -}}
{{- end -}}
{{- end -}}
{{- end -}}
```

- [ ] **Step 4: Run tests, verify pass**

Run: `helm unittest deploy/helm/coredns-crd`

Expected: PASS for all guard tests AND for the Task 6 service-lb tests that previously couldn't render. helm-unittest's `failedTemplate` matcher matches against the rendered failure message; the `errorPattern: open recursive resolver` substring is what makes the assertion green.

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/coredns-crd/templates/_helpers.tpl deploy/helm/coredns-crd/tests/recursion-guard_test.yaml
git commit -m "feat(chart): public-LB recursion guard prevents open resolver"
```

---

## Task 8: NOTES.txt update

Goal: extend `NOTES.txt` to detect standalone-shaped config (kubernetes plugin disabled) and emit a different post-install message pointing the user at ingress recipes.

**Files:**
- Modify: `deploy/helm/coredns-crd/templates/NOTES.txt`

NOTES.txt isn't covered by helm-unittest (templates is for resource manifests). We rely on visual review for this task.

- [ ] **Step 1: Read current NOTES.txt**

Run: `cat deploy/helm/coredns-crd/templates/NOTES.txt`

- [ ] **Step 2: Replace NOTES.txt with mode-aware version**

Overwrite `deploy/helm/coredns-crd/templates/NOTES.txt`:

```
coredns-crd installed.

{{- if include "coredns-crd.kubernetesEnabled" . }}

Mode: cluster-DNS replacement
The kubernetes plugin is enabled — this install owns cluster.local
resolution. Ensure your kubelet's --cluster-dns points at:
  ClusterIP: {{ default "<allocated by API server>" .Values.service.clusterIP }}
{{- else }}

Mode: standalone (declarative DNS server)
The kubernetes plugin is disabled — this install resolves only
DNSSlice records. The cluster's existing CoreDNS still owns
cluster.local resolution.

Ingress recipes:

  Stub-domain via main CoreDNS (most common):
    Forward a zone like internal.lan to our ClusterIP from the
    cluster's main CoreDNS Corefile. See README "Deployment modes"
    for kubeadm / k3s / RKE2 / Talos snippets.

  Per-pod dnsConfig:
    spec:
      dnsPolicy: None
      dnsConfig:
        nameservers: [<our-ClusterIP>]
        searches: [internal.lan]
{{- end }}

Service: {{ include "coredns-crd.fullname" . }} (namespace {{ .Release.Namespace }})
{{- with .Values.service.clusterIP }}
ClusterIP: {{ . }}
{{- end }}

{{- if .Values.service.loadBalancer.enabled }}

LoadBalancer Service: {{ include "coredns-crd.fullname" . }}-lb
  Get external IP:
    kubectl -n {{ .Release.Namespace }} get svc {{ include "coredns-crd.fullname" . }}-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
{{- if include "coredns-crd.forwardEnabled" . }}
{{- if .Values.corefile.forward.allowPublicRecursion }}

WARNING: corefile.forward.allowPublicRecursion is true and the
LoadBalancer is enabled with recursion. If this LB is reachable from
the public internet, you have shipped an open recursive resolver
(DDoS amplification risk). Confirm the LB is internal-only.
{{- end }}
{{- end }}
{{- end }}

{{- if .Values.hostNetwork.enabled }}

hostNetwork is enabled — the pod runs in the node's network namespace.
Each node IP is a DNS server on :53. NetworkPolicy does NOT apply to
hostNetwork pods.
{{- end }}

Verify resolution from a debug pod:

  kubectl run --rm -it dnstest --image=alpine:3.20 --restart=Never -- \
    sh -c "apk add --no-cache bind-tools && dig +short @{{ default "<service-cluster-ip>" .Values.service.clusterIP }} <name> <type>"

Apply an example DNSSlice:

  kubectl apply -f https://raw.githubusercontent.com/hbinhng/coredns-crd/main/config/example/dnsslice.yaml

{{- if .Values.networkPolicy.enabled }}

NetworkPolicy is enabled. Confirm your CNI enforces NetworkPolicy and
that networkPolicy.metricsAllowedFrom matches your Prometheus install
(default: app.kubernetes.io/name=prometheus in the monitoring namespace).
{{- end }}

Verify image signature:

  cosign verify {{ include "coredns-crd.image" . }} \
    --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

- [ ] **Step 3: Manually render and visually verify both modes**

Run:
```bash
helm template coredns-crd deploy/helm/coredns-crd > /tmp/notes-default.yaml
helm template coredns-crd deploy/helm/coredns-crd -f deploy/helm/coredns-crd/values-standalone.yaml > /tmp/notes-standalone.yaml
diff /tmp/notes-default.yaml /tmp/notes-standalone.yaml
```

The diff should show no difference for `NOTES.txt` (it's not rendered by `helm template`). Instead, run `helm install` against a real or KinD cluster and visually inspect the post-install message — but for a unit-style verification, run:

```bash
helm install --dry-run coredns-crd-test deploy/helm/coredns-crd | grep -A 30 "NOTES:"
helm install --dry-run coredns-crd-test deploy/helm/coredns-crd -f deploy/helm/coredns-crd/values-standalone.yaml | grep -A 30 "NOTES:"
```

Expected: first emits "Mode: cluster-DNS replacement"; second emits "Mode: standalone (declarative DNS server)" with ingress recipes.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/coredns-crd/templates/NOTES.txt
git commit -m "feat(chart): mode-aware NOTES.txt with ingress recipe pointers"
```

---

## Task 9: E2E standalone-mode phase

Goal: extend `test/e2e/run.sh` with a Scenario 5 that installs the chart a second time in `coredns-crd-standalone` namespace using the overlay file, applies a DNSSlice, and resolves it via a `dig` pod whose `dnsConfig.nameservers` points at the standalone install's ClusterIP. Confirms the original cluster DNS still works alongside.

**Files:**
- Modify: `test/e2e/run.sh`

- [ ] **Step 1: Read the current run.sh tail**

Run: `tail -80 test/e2e/run.sh`
Note the existing scenarios and the cleanup pattern.

- [ ] **Step 2: Add Scenario 5 to run.sh**

In `test/e2e/run.sh`, after the last existing scenario but BEFORE the `# end of scenarios` marker (or at the end if there isn't one), append:

```bash
phase "Scenario 5: Standalone-mode side-by-side install"
# Install the chart a second time in its own namespace using the
# values-standalone.yaml overlay. Verifies that:
#  (a) the overlay produces a Corefile without the kubernetes plugin
#      and without the forward block,
#  (b) the standalone install does not collide with the in-place
#      cluster-DNS install (different release name, different Lease,
#      different Service name),
#  (c) a pod with dnsConfig.nameservers pointing at the standalone
#      ClusterIP can resolve a DNSSlice that the standalone install
#      owns.

kubectl create namespace coredns-crd-standalone
helm install coredns-crd-standalone deploy/helm/coredns-crd \
  --namespace coredns-crd-standalone \
  -f deploy/helm/coredns-crd/values-standalone.yaml \
  --set image.repository=coredns-crd \
  --set image.tag=e2e \
  --set image.pullPolicy=IfNotPresent \
  --set replicaCount=1 \
  --set leaderElection.enabled=false \
  --set podDisruptionBudget.enabled=false \
  --set topologySpreadConstraints.enabled=false \
  --set priorityClassName=""

# replicaCount=1 + leader-election off keeps the test simple: one pod,
# no Lease churn. PDB off because PDB requires replicas>1. Topology
# constraints off because we're scheduling a single replica. priorityClassName
# emptied because system-cluster-critical requires kube-system or a
# ResourceQuota allowance — neither true in a tenant namespace.
# Tolerations are left as-is (they only widen scheduling, never narrow).

kubectl -n coredns-crd-standalone rollout status \
  deployment/coredns-crd-standalone --timeout=120s

STANDALONE_DNS_IP=$(kubectl -n coredns-crd-standalone get svc \
  coredns-crd-standalone -o jsonpath='{.spec.clusterIP}')
[[ -n "$STANDALONE_DNS_IP" ]] || { echo "standalone Service has no ClusterIP"; exit 1; }
echo "standalone DNS Service IP: $STANDALONE_DNS_IP"

# Apply a DNSSlice into the standalone namespace.
cat <<EOF | kubectl apply -f -
apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: standalone-test
  namespace: coredns-crd-standalone
spec:
  entries:
    - fqdn: standalone.example.test
      type: A
      a: 10.42.42.42
EOF
kubectl -n coredns-crd-standalone wait --for=condition=Ready \
  dnsslice/standalone-test --timeout=30s

# Resolve via a dig pod whose dnsConfig points at the standalone DNS.
# Uses jessie-dnsutils (dig pre-installed; avoids apk-on-restricted-PSA
# issues we hit during enigma multi-node validation).
kubectl run dig-standalone -n coredns-crd-standalone \
  --image=registry.k8s.io/e2e-test-images/jessie-dnsutils:1.7 \
  --restart=Never \
  --overrides="$(cat <<EOF
{
  "spec": {
    "dnsPolicy": "None",
    "dnsConfig": {"nameservers": ["$STANDALONE_DNS_IP"]},
    "containers": [{
      "name": "dig-standalone",
      "image": "registry.k8s.io/e2e-test-images/jessie-dnsutils:1.7",
      "command": ["sh", "-c", "dig +short standalone.example.test A; dig +short kubernetes.default.svc.cluster.local A"]
    }]
  }
}
EOF
)" \
  --command -- sh -c 'true'

PHASE=""
for i in $(seq 1 30); do
  PHASE=$(kubectl -n coredns-crd-standalone get pod dig-standalone \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Succeeded" || "$PHASE" == "Failed" ]] && break
  sleep 1
done
DIG_OUT=$(kubectl -n coredns-crd-standalone logs dig-standalone 2>/dev/null)
echo "$DIG_OUT"
kubectl -n coredns-crd-standalone delete pod dig-standalone --wait=false 2>/dev/null || true

# Standalone DNS resolves the declared record:
grep -q '10.42.42.42' <<<"$DIG_OUT" || { echo "standalone DNS did not resolve standalone.example.test"; exit 1; }
# Standalone DNS does NOT resolve cluster.local (kubernetes plugin is
# disabled). The second `dig` should produce empty output.
grep -q '^[0-9]' <<<"$(echo "$DIG_OUT" | tail -1)" && {
  echo "standalone DNS unexpectedly resolved cluster.local — kubernetes plugin should be disabled"
  exit 1
} || true

# Cleanup standalone install.
helm uninstall coredns-crd-standalone -n coredns-crd-standalone --wait
kubectl delete namespace coredns-crd-standalone --wait=false
echo "Scenario 5 PASS"
```

- [ ] **Step 3: Test locally if KinD is available**

Run:
```bash
make e2e   # if a Makefile target exists
# or:
bash test/e2e/run.sh
```

Expected: all scenarios PASS, including Scenario 5. If you don't have a KinD cluster locally, skip and let CI verify (Step 5).

- [ ] **Step 4: Commit**

```bash
git add test/e2e/run.sh
git commit -m "test(e2e): add standalone-mode side-by-side scenario"
```

- [ ] **Step 5: Push to a branch and verify the e2e GH Actions run passes**

Run:
```bash
git push -u origin <branch-name>
gh run watch
```

Expected: green E2E run including Scenario 5 output. If it fails, read logs via `gh run view --log-failed`.

---

## Task 10: README deployment-modes section + ingress recipes

Goal: rewrite the README's install section to document both deployment modes, with copy-pasteable ingress recipes for stub-domain (× four distros), per-pod dnsConfig, hostNetwork, LoadBalancer (cloud + bare-metal), and NodePort.

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Read current README install section**

Run: `cat README.md`
Identify the existing install section to know where to splice the new content.

- [ ] **Step 2: Rewrite/extend the install section**

In `README.md`, replace the existing install section with the following. (Adjust headings to match current README style — preserve the surrounding sections like overview, status, performance.)

````markdown
## Deployment modes

coredns-crd ships two deployment shapes:

1. **Cluster-DNS replacement** *(default)* — coredns-crd becomes the cluster's DNS server. Replaces the in-cluster CoreDNS. Single source of truth for `cluster.local` and DNSSlice records.
2. **Standalone DNS server** *(opt-in via overlay)* — coredns-crd runs side-by-side with the cluster's existing CoreDNS as a declarative authoritative DNS server. The cluster's CoreDNS still owns `cluster.local`; coredns-crd answers only for the FQDNs declared in DNSSlice CRDs.

### Mode 1: Cluster-DNS replacement

```bash
helm install coredns-crd oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  --namespace kube-system \
  --set fullnameOverride=kube-dns \
  --set service.clusterIP=10.96.0.10   # match your cluster's --cluster-dns
```

Replace `10.96.0.10` with `kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'`. The chart owns the `kube-dns` Service and ConfigMap; you must remove the cluster's existing `coredns` Deployment first (or the install will collide).

### Mode 2: Standalone DNS server

```bash
helm install coredns-crd oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  --namespace coredns-crd \
  --create-namespace \
  -f deploy/helm/coredns-crd/values-standalone.yaml
```

The overlay disables the `kubernetes` plugin (cluster's CoreDNS owns `cluster.local`) and disables forwarding (authoritative-only). Apps that should resolve via coredns-crd need an ingress path — pick from the recipes below.

#### Ingress recipes (Mode 2)

##### Stub-domain via the cluster's main CoreDNS *(recommended)*

The cluster's main CoreDNS forwards a zone to coredns-crd. Apps need zero changes — `foo.internal.lan` just resolves.

Get coredns-crd's ClusterIP:

```bash
DNS_IP=$(kubectl -n coredns-crd get svc coredns-crd -o jsonpath='{.spec.clusterIP}')
echo $DNS_IP
```

Add this stanza to your cluster's main CoreDNS Corefile:

```
internal.lan:53 {
  forward . <DNS_IP>
}
```

Distro-specific snippets:

- **kubeadm / KinD / Talos:** edit the `kube-system/coredns` ConfigMap directly:
  ```bash
  kubectl -n kube-system edit configmap coredns
  # Add the stanza above, then:
  kubectl -n kube-system rollout restart deployment/coredns
  ```

- **k3s:** create a `coredns-custom` ConfigMap (k3s's CoreDNS reads it automatically):
  ```yaml
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: coredns-custom
    namespace: kube-system
  data:
    internal.server: |
      internal.lan:53 {
        forward . <DNS_IP>
      }
  ```

- **RKE2:** create a `HelmChartConfig` to patch `rke2-coredns`:
  ```yaml
  apiVersion: helm.cattle.io/v1
  kind: HelmChartConfig
  metadata:
    name: rke2-coredns
    namespace: kube-system
  spec:
    valuesContent: |-
      servers:
      - zones:
        - zone: .
        port: 53
        plugins:
        - name: errors
        - name: health
        - name: ready
        - name: kubernetes
          parameters: cluster.local in-addr.arpa ip6.arpa
          configBlock: |-
            pods insecure
            fallthrough in-addr.arpa ip6.arpa
            ttl 30
        - name: prometheus
          parameters: 0.0.0.0:9153
        - name: forward
          parameters: . /etc/resolv.conf
        - name: cache
          parameters: 30
        - name: loop
        - name: reload
        - name: loadbalance
      - zones:
        - zone: internal.lan
        port: 53
        plugins:
        - name: forward
          parameters: . <DNS_IP>
  ```

##### Per-pod `dnsConfig.nameservers`

When you can't touch the cluster's main CoreDNS:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: app
spec:
  dnsPolicy: None
  dnsConfig:
    nameservers: [<DNS_IP>]
    searches: [internal.lan, svc.cluster.local, cluster.local]
    options:
      - name: ndots
        value: "5"
  containers:
    - name: app
      image: my-app:latest
```

##### hostNetwork (node IPs as DNS servers)

For homelab, bare-metal, or any "my router uses node IPs as upstream DNS" setup:

```bash
helm install coredns-crd oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  -f deploy/helm/coredns-crd/values-standalone.yaml \
  --set hostNetwork.enabled=true
```

Each node's IP becomes a DNS server on `:53`. NetworkPolicy does NOT apply to hostNetwork pods. Replicas must not exceed the number of schedulable nodes (port 53 conflicts).

##### LoadBalancer Service (out-of-cluster clients)

For internal LANs, other clusters, or external devices:

```bash
helm install coredns-crd oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  -f deploy/helm/coredns-crd/values-standalone.yaml \
  --set service.loadBalancer.enabled=true \
  --set 'service.loadBalancer.annotations.networking\.gke\.io/load-balancer-type=Internal'
```

Cloud-provider annotations:

| Cloud | Annotation |
|---|---|
| GKE | `networking.gke.io/load-balancer-type: Internal` |
| EKS | `service.beta.kubernetes.io/aws-load-balancer-internal: "true"` and `service.beta.kubernetes.io/aws-load-balancer-type: nlb` (NLB handles UDP+TCP) |
| AKS | `service.beta.kubernetes.io/azure-load-balancer-internal: "true"` |
| MetalLB | `metallb.io/loadBalancerIPs: 192.168.1.53` |

Sharp edges:
- **UDP+TCP on the same LB IP**: AWS classic ELB does not support mixed-protocol Services. Use NLB. GCP requires separate forwarding rules; the rendered Service works on GKE but you may need a second Service-per-protocol on bare GCP TCP/UDP LBs.
- **Public exposure**: if your LB is reachable from the public internet, do NOT enable `corefile.forward.upstreams` (open recursive resolver = DDoS amplification). The chart enforces this with a render-time guard; setting `corefile.forward.allowPublicRecursion: true` opts out of the guard, only do this for internal-only LBs.

##### NodePort

```bash
helm install coredns-crd oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  -f deploy/helm/coredns-crd/values-standalone.yaml \
  --set service.type=NodePort
```

The default NodePort range is `30000–32767`, which is wrong for clients expecting `:53`. Pinning to `:53` requires `--service-node-port-range=53-32767` cluster-side. Most clusters don't widen this; prefer hostNetwork or LoadBalancer.

#### Recipe matrix

| Use case | Recommended ingress |
|---|---|
| "I want my own zone alongside `cluster.local`" | Stub-domain |
| "Platform team won't let me touch the main CoreDNS" | Per-pod `dnsConfig` |
| "Homelab / bare-metal node IPs as DNS servers" | hostNetwork |
| "Other clusters / LAN devices need to query us" | LoadBalancer (internal) |
| "Public/external DNS authority" | LoadBalancer (public, **no recursion**) |
````

- [ ] **Step 3: Verify the README renders**

Run:
```bash
glow README.md   # if installed
# or just visually inspect
less README.md
```

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs(readme): document standalone mode + ingress recipes"
```

---

## Task 11: CHANGELOG v0.2.0 stub

Goal: add an unreleased v0.2.0 entry to `CHANGELOG.md`.

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `deploy/helm/coredns-crd/Chart.yaml`

- [ ] **Step 1: Bump Chart.yaml version + appVersion**

In `deploy/helm/coredns-crd/Chart.yaml`, change:

```yaml
version: 0.1.0
appVersion: "v0.1.0"
```

to:

```yaml
version: 0.2.0
appVersion: "v0.2.0"
```

(The release pipeline overwrites these from `${GITHUB_REF_NAME}` at tag time anyway, but the in-repo values should reflect the next planned version.)

- [ ] **Step 2: Add v0.2.0 unreleased entry to CHANGELOG.md**

In `CHANGELOG.md`, after the header but before the `## [0.1.0]` section, insert:

```markdown
## [0.2.0] — Unreleased

### Added

- Standalone deployment mode via `values-standalone.yaml` overlay:
  the chart can now run side-by-side with the cluster's existing
  CoreDNS as a declarative authoritative DNS server, rather than as
  a tier-0 cluster-DNS replacement. The overlay disables the
  `kubernetes` plugin and disables forwarding by default
  (authoritative-only behavior).
- `service.loadBalancer` values block — opt-in second Service of
  type LoadBalancer for out-of-cluster DNS clients. Defaults to
  `externalTrafficPolicy: Local` for source-IP preservation.
- `hostNetwork.{enabled, dnsPolicy}` values — opt-in hostNetwork mode
  so each node IP becomes a DNS server on `:53`. Requires the
  existing `cap_net_bind_service+ep` setcap shipped in v0.1.0.
- Public-LB recursion guard: `helm install` fails at render time when
  `service.loadBalancer.enabled: true` is combined with non-empty
  `corefile.forward.upstreams` and `corefile.forward.allowPublicRecursion`
  is false. Prevents accidentally shipping an open recursive resolver.
- `corefile.forward.upstreams` (list) — preferred replacement for the
  legacy `corefile.forward.upstream` (string). The legacy field still
  works via a back-compat helper.
- helm-unittest test suite under `deploy/helm/coredns-crd/tests/`,
  wired into CI. Locks default-install rendering as a regression
  baseline and exercises every new gate.
- README "Deployment modes" section with stub-domain integration
  recipes for kubeadm, k3s, RKE2, and Talos.

### Changed

- The `forward .` block in the rendered Corefile is now conditional
  on `corefile.forward.upstreams` (or legacy `upstream`) being
  non-empty. Previously the block was unconditional, which would have
  rendered an invalid Corefile if `upstream` was set to an empty string.

[0.2.0]: https://github.com/hbinhng/coredns-crd/releases/tag/v0.2.0
```

- [ ] **Step 3: Commit**

```bash
git add CHANGELOG.md deploy/helm/coredns-crd/Chart.yaml
git commit -m "docs(changelog): v0.2.0 unreleased entry + chart version bump"
```

---

## Task 12: Independent agentic validation

Goal: spawn a fresh `code-reviewer` agent, hand it the spec + diff + success criteria, and treat its findings as merge gates. Independent because *I* (the implementer) have context-bias; an agent reading the artifact cold pushes back on what doesn't add up.

**Files:** none modified by this task; possibly modified by the followup fixes if the agent flags issues.

- [ ] **Step 1: Confirm the working tree is clean and on the correct branch**

Run:
```bash
git status
git log --oneline main..HEAD
```

Expected: clean working tree, ~10 commits on the feature branch (one per task).

- [ ] **Step 2: Run the regression checks locally**

Run:
```bash
go test -race -count=2 -cover ./...
helm unittest deploy/helm/coredns-crd
helm lint deploy/helm/coredns-crd
helm lint deploy/helm/coredns-crd -f deploy/helm/coredns-crd/values-standalone.yaml
helm template coredns-crd deploy/helm/coredns-crd > /tmp/render-default.yaml
helm template coredns-crd deploy/helm/coredns-crd -f deploy/helm/coredns-crd/values-standalone.yaml > /tmp/render-standalone.yaml
diff /tmp/render-default.yaml /tmp/render-standalone.yaml | less
```

Expected: all green. The diff between default and standalone renders should show:
- ConfigMap data.Corefile: standalone is missing the `kubernetes` block and missing the `forward .` block
- No other resource differs structurally

If `go test -cover` shows any package below its baseline, fix coverage before invoking the agent.

- [ ] **Step 3: Spawn the verification agent**

Use the Agent tool with `subagent_type: superpowers:code-reviewer` (or `general-purpose` if that doesn't exist). Prompt:

```
Independent verification of the standalone-mode implementation in
coredns-crd. Spec at:
  docs/superpowers/specs/2026-04-30-standalone-mode-design.md
Plan at:
  docs/superpowers/plans/2026-04-30-standalone-mode.md
Implementation diff:
  git log --oneline main..HEAD  (~10 commits)
  git diff main..HEAD

Verify:
1. Every "Net-new chart additions" item from the spec is present in
   the implementation. List anything missing.
2. The public-LB recursion guard fires for ALL combinations described
   in the spec. Walk through each combination and confirm the test
   covers it.
3. The back-compat helper for `corefile.forward.upstream` (legacy
   string) actually preserves v0.1.0 rendering byte-for-byte for an
   unmodified values.yaml. Run `helm template` against the chart at
   the current HEAD and against the chart at v0.1.0 (git checkout
   the v0.1.0 tag in a worktree) — diff the rendered ConfigMap.
4. The values-standalone.yaml overlay does NOT regress the default
   install. Run `helm unittest` and confirm the baseline tests pass.
5. The hostNetwork code path does the right thing — `dnsPolicy` is
   set correctly, `hostNetwork: true` is emitted only when enabled,
   and there is no surprising interaction with the existing
   securityContext / capabilities block.
6. Edge cases the implementer might have rationalized away:
   - What if a user sets BOTH `corefile.forward.upstreams: []` AND
     `corefile.forward.upstream: /etc/resolv.conf`? Which wins?
   - What if a user sets `service.loadBalancer.enabled: true` AND
     `hostNetwork.enabled: true`? Does the LB Service still target
     the hostNetwork pods? Is that even sensible?
   - What if `corefile.forward.allowPublicRecursion: true` is set
     but `service.loadBalancer.enabled: false`? Does the guard
     correctly *not* fire?
7. Run `go test -race -count=2 -cover ./...` and confirm coverage
   for every package matches or exceeds what was committed before
   this branch's first commit. Report any drop.
8. Re-read the spec's "Out of scope" list. Did the implementation
   accidentally add any of those?

Report findings as a numbered list. Distinguish:
- BLOCKER (must fix before merge)
- NIT (consistency / style / docs)
- QUESTION (genuine ambiguity to discuss)

Be honest. The implementer wants real pushback, not a rubber-stamp.
```

- [ ] **Step 4: Address findings**

For each BLOCKER: fix in a new commit on the same branch.
For each NIT: fix or document why it's deferred.
For each QUESTION: respond inline (in the chat with the user), don't decide unilaterally.

- [ ] **Step 5: Re-run the verification once any BLOCKERs are fixed**

If any BLOCKERs were addressed, spawn a *second* fresh agent (or send `Continue with the same review against the latest commit on the branch.` to the same agent) to confirm fixes are correct. Don't skip this — fixes can introduce new bugs.

- [ ] **Step 6: Final commit summary**

Once the agent reports no remaining BLOCKERs, write a final summary message to the user:

```
Standalone-mode implementation complete on branch <branch-name>.
- N commits, all atomic
- helm-unittest passes (X tests, Y asserts)
- Existing E2E + new Scenario 5 pass on KinD
- Independent verification agent flagged Z findings; all addressed
- Ready to merge to main and tag v0.2.0
```

---

## Notes

### Commit cadence
Per project convention (`feedback_atomic_commits.md`): one logical change per commit. Each task above produces one commit. If a task's TDD cycle naturally wants to be split (e.g. test commit then implementation commit), that's also fine — but don't skip commits.

### Co-author trailer
Per `feedback_no_coauthor.md`: do NOT include `Co-Authored-By:` in commit messages. The example commit messages above are correct as written.

### When in doubt
- Coverage regressions are blockers, not nits.
- The recursion guard is intentional friction. If you find yourself wanting to soften it ("just make it a NOTES warning"), reread the spec rationale before touching it.
- The back-compat helper for `corefile.forward.upstream` is a one-release courtesy. v0.3.0 can drop the legacy string field; don't extend it further.
