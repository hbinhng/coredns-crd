# coredns-crd

CoreDNS as cluster DNS, with **DNS-as-code via the `DNSSlice` CRD**. Define
records in YAML, `kubectl apply`, resolve through the cluster's DNS
service. No ConfigMap edits, no CoreDNS restarts.

```yaml
apiVersion: dns.coredns-crd.io/v1alpha1
kind: DNSSlice
metadata:
  name: web
  namespace: default
spec:
  defaultTTL: 300
  entries:
    - name: web.example.com.
      type: A
      a: { address: 10.0.0.1 }
    - name: alias.example.com.
      type: CNAME
      cname: { target: web.example.com. }
    - name: _http._tcp.example.com.
      type: SRV
      srv: { priority: 10, weight: 100, port: 80, target: web.example.com. }
```

```
$ dig +short @<cluster-dns> web.example.com
10.0.0.1
```

## What it is

A CoreDNS plugin (`crd`) plus a `DNSSlice` CRD. Pods publish records by
creating `DNSSlice` objects; the plugin watches them, indexes
`(FQDN, type) → RR`, and answers queries from memory. The standard
`kubernetes` plugin is chained for `cluster.local` resolution, so this
chart can fully replace `kube-dns`/`coredns` as cluster DNS.

Supported record types: **A, AAAA, CNAME, TXT, SRV**, plus a `Raw`
escape hatch for arbitrary RFC 1035 strings.

## Install

```
helm install coredns-crd \
  oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  --version 0.1.0 \
  --namespace kube-system \
  --set service.clusterIP=10.96.0.10        # kubeadm; k3s uses 10.43.0.10
```

To replace the cluster's existing DNS Service, set
`fullnameOverride: kube-dns`. See the
[chart README](deploy/helm/coredns-crd/README.md) for the full values
reference, replacement instructions, and hardening notes (NetworkPolicy,
RBAC scoping).

## How it works

- **Watch:** a dynamic informer streams `DNSSlice` objects.
- **Index:** an in-memory map keyed by `(FQDN, type)` is rebuilt on each
  upsert. Lookups are O(1) — index size doesn't affect the hot path.
- **Arbitrate:** when two slices claim the same `(FQDN, type)`, the
  oldest `creationTimestamp` wins (UID as tiebreak). Losers flip
  `Conflicting=True` in their status and a `ConflictDetected` Event is
  emitted; the cluster keeps serving the winner.
- **HA:** `replicaCount ≥ 2` enables leader election via a `Lease`. Only
  the leader writes status; all replicas serve DNS. Failover is < 5 s.
- **Observability:** eight `coredns_crd_*` Prometheus series (lookups,
  applies, status patches, conflicts, index size, leader gauge) on
  `:9153/metrics`.

## Status

API is `v1alpha1`. The shape is intentionally narrow and stable —
adding record types is additive, the polymorphic envelope is the long-
term schema. v1beta1 graduation is planned once a v1.0 user has run it
in production for a release cycle.

## Performance

A single 4-vCPU node sustains **~57k QPS** with 0% loss against a
25,000-record index, holding ~100 MiB per replica. See
[`docs/benchmarks/2026-04-30-baseline.md`](docs/benchmarks/2026-04-30-baseline.md)
for method, scaling notes, and recommendations beyond ~50k records.

## Provenance

Images and chart are signed with cosign keyless OIDC. To verify:

```
cosign verify ghcr.io/hbinhng/coredns-crd:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Development

- `make test` — race-clean unit tests across `internal/*` and `plugin/crd`.
- `make build` — builds the plugin standalone.
- `make image` — builds the CoreDNS+plugin image.
- `bash test/e2e/run.sh` — runs the same scenarios CI runs against any
  current-context cluster (KinD, k3s, etc.). See
  [`test/e2e/README.md`](test/e2e/README.md).
- `bash test/load/seed.sh <N> && bash test/load/run.sh <duration>` —
  dnsperf load harness.

## Repo layout

```
api/v1alpha1/    DNSSlice types + deepcopy
internal/index/  LWW arbitration, snapshot, index
internal/leader/ leader-election wrapper
internal/events/ ConflictDetected/Resolved emitter
internal/metrics/ Prometheus collectors
plugin/crd/      CoreDNS plugin: setup, ServeDNS, status writer
deploy/helm/     Helm chart (CRD installs from chart/crds/)
test/e2e/        end-to-end scenarios
test/load/       dnsperf harness
docs/            specs, plans, benchmarks
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
