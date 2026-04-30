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

See the [chart README](deploy/helm/coredns-crd/README.md) for the full values reference and hardening notes (NetworkPolicy, RBAC scoping).

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
cosign verify ghcr.io/hbinhng/coredns-crd:v0.2.0 \
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
