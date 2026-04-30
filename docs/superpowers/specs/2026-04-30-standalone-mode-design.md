# Standalone DNS server mode

**Status:** approved 2026-04-30
**Target version:** v0.2.0 (minor — additive, no breaking changes)

## Problem

The chart today ships one deployment shape: replace the cluster's CoreDNS
and become the tier-0 DNS authority for `cluster.local`. That's the right
default for greenfield clusters but the wrong default for established
clusters where the platform team owns CoreDNS and a tenant just wants a
declarative authoritative DNS server alongside it. We need a second
deployment mode that treats coredns-crd as a regular DNS server — not a
cluster component.

## Goals

- Add a `mode: standalone` deployment that runs side-by-side with the
  cluster's existing CoreDNS without conflicting with it.
- Cover the realistic ingress paths: stub-domain integration with the
  cluster CoreDNS, per-pod `dnsConfig`, hostNetwork, LoadBalancer, NodePort.
- Keep the existing `mode: cluster-dns` users on a no-change upgrade path.
- Single chart, single image, single CRD — no fork.

## Non-goals

- Automating the cluster-CoreDNS stub-domain edit. That varies by distro
  (kubeadm / k3s / RKE2 / Talos) and lives outside our release scope.
  Document four recipes; don't ship a controller for it.
- Recursive resolver as a default. Standalone mode is **authoritative-only
  by default**; recursion is opt-in.
- Public DNS authority hardening (DNSSEC, RRL, BIND-style ACLs). Out of
  scope for v0.2.0.

## Design

### Mode umbrella value

A single top-level `mode: cluster-dns | standalone` value flips the
defaults for the four sub-values that matter. Underlying values stay
overridable so power users can mix-and-match.

| Concern | `cluster-dns` (default, existing) | `standalone` (new) |
|---|---|---|
| Corefile zone | `.:53` | `.:53` |
| `kubernetes` plugin | enabled | disabled |
| `forward` block | enabled (`/etc/resolv.conf`) | disabled by default; opt in via `corefile.forward.upstreams` |
| Service `clusterIP` | empty (or pinned to kube-dns IP) | empty (auto-allocate) |
| `fullnameOverride` | empty (or `kube-dns`) | empty |

`mode` is a UX shortcut. It doesn't introduce new state — it just
rewrites the defaults of values that already exist (or are added by this
spec). Users can still set sub-values individually and override the mode's
defaults.

### Plugin code

**No changes.** The `crd` plugin doesn't hardcode zones — CoreDNS routes
queries by Corefile zone matching, the plugin handles whatever it sees.
Verified by grep: `cluster.local` appears nowhere in `plugin/crd/*.go`.
Pure chart + docs work.

### Ingress paths

Five distinct paths, each a real use case. The chart makes all five
tractable; the README explains when to pick which.

**1. Stub-domain via cluster's main CoreDNS** *(headline use case)*

The most common "side-by-side DNS" pattern. The cluster's main CoreDNS
forwards a specific zone to our ClusterIP. Apps need zero changes —
`foo.internal.lan` just resolves.

```
internal.lan:53 {
  forward . <coredns-crd ClusterIP>
}
```

How users wire this up varies by distro; the chart cannot automate it
(it's outside our release scope and varies per distro). The README must
ship copy-pasteable recipes for kubeadm/KinD, k3s, RKE2, and Talos, plus
the `kubectl get svc -o jsonpath='{.spec.clusterIP}'` one-liner.

**2. Per-pod `dnsConfig.nameservers`**

When stub-domain editing is off-limits (locked-down platform, multi-
tenant). Per-pod opt-in:

```yaml
dnsPolicy: None
dnsConfig:
  nameservers: [<coredns-crd ClusterIP>]
  searches: [internal.lan]
```

Documented as the fallback. README needs an "if you can't touch the
cluster's CoreDNS" subsection.

**3. hostNetwork mode**

Pod runs in the node's network namespace; node IPs become DNS servers
on :53 directly. No LB, no NodePort high-port. Common for homelab,
bare-metal, "my router uses these IPs as upstream DNS." Already works
with our `cap_net_bind_service+ep` setcap.

New chart values:

```yaml
hostNetwork:
  enabled: false
  dnsPolicy: ClusterFirstWithHostNet
```

Caveats called out in README: NetworkPolicy doesn't apply to hostNetwork
pods; replicas-per-node port-53 conflict (existing topology spread +
anti-affinity handles this); Talos requires the host-net pod-security
profile.

**4. LoadBalancer Service**

For out-of-cluster clients on the LAN or other clusters.

New chart values:

```yaml
service:
  loadBalancer:
    enabled: false
    annotations: {}
    loadBalancerClass: ""
    externalTrafficPolicy: Local  # source IP preservation
    loadBalancerIP: ""
```

New template: `templates/service-lb.yaml`, gated on
`service.loadBalancer.enabled`. Same selectors as the ClusterIP Service
(both point to the same Deployment).

Documented sharp edges:
- **UDP+TCP on the same LB IP**: AWS NLB handles it; classic ELB does
  not; GCP requires separate forwarding rules. README ships a
  "split-Services" recipe (two Services, one per protocol) for affected
  providers.
- **`externalTrafficPolicy: Local` is the default** — DNS policies that
  care about source IP need it; SNAT loses the original client.
- **Bare-metal**: MetalLB / kube-vip / Cilium L2 announcement — works
  but the user supplies the LB controller. Document, don't bundle.
- **Public exposure with recursion enabled = open resolver** (DDoS
  reflector). The chart must guard against this combination — see
  *Public-LB recursion guard* below.

**5. NodePort**

Standard `service.type: NodePort` works as-is. The default NodePort range
(30000–32767) is wrong for DNS clients that expect :53; pinning
`nodePort: 53` requires `--service-node-port-range` to be widened
cluster-side, which most users won't do. Document briefly with the
caveat; no chart machinery beyond what already exists.

### Public-LB recursion guard

If `service.loadBalancer.enabled: true` and `corefile.forward.upstreams`
is non-empty and the user has not opted in by setting
`corefile.forward.allowPublicRecursion: true`, the chart `fail`s at
template-render time with a clear message:

> Refusing to render: enabling `service.loadBalancer` together with
> `corefile.forward.upstreams` would expose an open recursive resolver.
> Set `corefile.forward.allowPublicRecursion: true` to confirm this is
> an internal LB (e.g. via `service.loadBalancer.annotations` for an
> internal scheme), or remove `corefile.forward.upstreams`.

The guard fires at `helm template` / `helm install` time, not at runtime.
This is friction by design — the failure mode of a misconfigured open
resolver (DDoS amplification, $cloud-bill events) outweighs the
ergonomics cost of the explicit opt-in.

### Net-new chart additions

1. `mode: cluster-dns | standalone` (default `cluster-dns`)
2. `service.loadBalancer.{enabled, annotations, loadBalancerClass, externalTrafficPolicy, loadBalancerIP}`
3. `hostNetwork.{enabled, dnsPolicy}`
4. `corefile.forward.upstreams: []` (list) replaces `corefile.forward.upstream` (string). Back-compat helper in `_helpers.tpl` resolves the old string into the new list so existing values files keep working.
5. `corefile.forward.allowPublicRecursion: false` (guard opt-out)

### Template changes

- **`configmap.yaml`** — wrap the `kubernetes` block in
  `coredns-crd.kubernetesEnabled` helper, wrap the `forward` block in
  `coredns-crd.forwardEnabled` helper. Today the `forward .` line is
  unconditional and emits a broken Corefile if `upstream` is empty —
  this is also a latent bug fix.
- **`deployment.yaml`** — add `hostNetwork: {{ .Values.hostNetwork.enabled }}`
  and `dnsPolicy: {{ .Values.hostNetwork.dnsPolicy }}` (gated).
- **`service.yaml`** — unchanged.
- **`service-lb.yaml`** — new, gated on `service.loadBalancer.enabled`.
- **`_helpers.tpl`** — new helpers: `coredns-crd.kubernetesEnabled`,
  `coredns-crd.forwardEnabled`, `coredns-crd.forwardUpstreams`
  (back-compat resolver), `coredns-crd.publicLBRecursionGuard`.
- **`NOTES.txt`** — emit the active mode + ingress recipe pointer for
  the user's chosen Service type.

### Documentation

New top-level README section: **Deployment modes**. Two sub-sections,
one per mode, each with:
- One-paragraph "when to pick this mode."
- Minimal install example.
- Ingress recipes (stub-domain × four distros, dnsConfig, hostNetwork,
  LoadBalancer, NodePort).
- A recipe matrix table (use case → recommended ingress).

Update CHANGELOG with a v0.2.0 stub entry now (not for release yet).

## Testing

### Coverage requirement

**100% line+branch coverage** on any new Go code. No new Go code is
strictly required by this spec (it's a chart change), but if any
helper logic moves into Go (e.g., a values validator), it ships at
100%. See `feedback_code_coverage.md`.

### Helm template-render tests

New unit-test directory: `deploy/helm/coredns-crd/tests/`. Use
[`helm-unittest`](https://github.com/helm-unittest/helm-unittest) (Helm
plugin — the de-facto standard for chart-template testing). Wire into CI
as a new step in the existing `test` job. Tests assert:

1. `mode: cluster-dns` (default) renders Corefile *with* `kubernetes`
   and *with* `forward .`.
2. `mode: standalone` renders Corefile *without* `kubernetes` and
   *without* `forward`.
3. `mode: standalone` + `corefile.forward.upstreams: [1.1.1.1]` renders
   `forward . 1.1.1.1`.
4. `service.loadBalancer.enabled: true` renders a second Service of
   type LoadBalancer with `externalTrafficPolicy: Local` by default.
5. `hostNetwork.enabled: true` renders Deployment with `hostNetwork: true`
   and `dnsPolicy: ClusterFirstWithHostNet`.
6. Public-LB recursion guard fires when `loadBalancer.enabled: true` +
   `forward.upstreams: [1.1.1.1]` + `allowPublicRecursion: false`.
7. Public-LB recursion guard does *not* fire when
   `allowPublicRecursion: true`.
8. Back-compat: old `corefile.forward.upstream: /etc/resolv.conf` string
   resolves into the new `upstreams` list and renders identically to
   v0.1.0.

### E2E

Extend `test/e2e/run.sh` with a second test phase:

- Install the chart a second time in `coredns-crd-standalone` namespace
  with `mode: standalone, corefile.forward.upstreams: []`.
- Apply a DNSSlice for `foo.internal.lan A 10.0.0.1`.
- Run a `dig` pod in the same namespace with
  `dnsConfig.nameservers: [<standalone ClusterIP>]` and resolve
  `foo.internal.lan` — expect `10.0.0.1`.
- Run a second `dig` against `cluster.local/kubernetes.default` from
  the same pod via the *original* cluster DNS — expect normal cluster
  DNS resolution (regression check that we didn't break anything).
- Verify both modes coexist (no Service collision, no Lease collision —
  the standalone install uses a different release name and therefore a
  different Lease name; assert this in the test).

### Independent agentic validation

After implementation, spawn a fresh verification agent (`code-reviewer`
or `general-purpose`) with:
- This spec path
- The implementation diff
- The success criteria from this spec

Agent reports back on spec compliance, edge-case handling, and test
coverage. Treat findings as gates, not suggestions. See
`feedback_agentic_validation.md`.

## Versioning

`v0.2.0`. Minor bump — purely additive. Existing values files render
identically (`mode` defaults to `cluster-dns`; the old
`corefile.forward.upstream` string is preserved via back-compat helper).

## Migration

`v0.1.x` users: nothing. Default behavior is preserved.

`v0.2.0` users adopting standalone: documented values snippet:

```yaml
mode: standalone
service:
  loadBalancer:
    enabled: true
    annotations:
      networking.gke.io/load-balancer-type: Internal
# or:
hostNetwork:
  enabled: true
```

## Out of scope (deferred)

- DNSSEC signing of declared records.
- Response-rate-limiting / RRL.
- DNS-over-TLS / DNS-over-HTTPS frontends.
- A controller that auto-edits the cluster's CoreDNS ConfigMap to wire
  up the stub-domain. (Tempting; punted because four distros × edge cases
  × admission webhooks is a project of its own.)
- Multi-zone authority scoping (`dns.zones: [internal.lan, prod.local]`
  with strict zone-boundary enforcement). The catch-all `.` zone covers
  the v0.2.0 use cases; revisit if multi-tenant zone isolation becomes
  a real ask.
