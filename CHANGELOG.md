# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). API stability
is governed by the CRD's `v1alpha1` designation: backwards-compatible
additions only, no guarantee of stability across minor versions.

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

## [0.1.0] — 2026-04-30

Initial release.

### Added

- `DNSSlice` CRD (`dns.coredns-crd.io/v1alpha1`) with polymorphic
  per-type entries: A, AAAA, CNAME, TXT, SRV, plus a `Raw` escape hatch.
  Namespaced scope, 50-entry per-slice cap.
- CoreDNS `crd` plugin: dynamic informer, in-memory `(FQDN, type) → RR`
  index, O(1) lookups, chained with the standard `kubernetes` plugin
  for `cluster.local` resolution.
- Last-Write-Wins arbitration on `(FQDN, type)` collisions: oldest
  `creationTimestamp` wins, UID tiebreak. Losers flip
  `Conflicting=True` and emit `ConflictDetected` Events.
- Leader election via `coordination.k8s.io/Lease`. All replicas serve
  DNS; only the leader writes `DNSSlice` status. Failover < 5 s.
- Helm chart (`oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd`)
  with HA defaults (2 replicas, PodDisruptionBudget, topology spread),
  optional NetworkPolicy, scoped RBAC keyed off Corefile contents.
- Eight Prometheus collectors on `:9153/metrics`: lookups, applies,
  status patches, conflict transitions, index size, leader gauge.
- KinD-based e2e workflow covering install, A/AAAA/CNAME/TXT/SRV
  resolution, metrics, conflict arbitration, and leader failover.
- dnsperf load harness; baseline at
  [`docs/benchmarks/2026-04-30-baseline.md`](docs/benchmarks/2026-04-30-baseline.md):
  ~57k QPS sustained on a 4-vCPU node, flat across 50–25,000 records,
  0% loss.
- Tag-driven release pipeline: cosign-signed image and chart published
  to GHCR, GitHub Release with auto-generated notes.
- File capability `cap_net_bind_service+ep` on `/coredns` so non-root
  pods can bind privileged port 53 on container runtimes that don't
  promote requested capabilities to the ambient set (KinD's containerd
  default; necessary on Talos containerd 2.1.6 + kube-OVN as well).
- Multi-node validation against a 3-CP Talos / kube-OVN cluster:
  topology spread, cross-node DNS over Geneve overlay, cross-node
  leader failover (~2 s), node-loss recovery (~14 s).

[0.1.0]: https://github.com/hbinhng/coredns-crd/releases/tag/v0.1.0
