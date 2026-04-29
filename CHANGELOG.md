# Changelog

All notable changes to this project are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). API stability
is governed by the CRD's `v1alpha1` designation: backwards-compatible
additions only, no guarantee of stability across minor versions.

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

[0.1.0]: https://github.com/hbinhng/coredns-crd/releases/tag/v0.1.0
