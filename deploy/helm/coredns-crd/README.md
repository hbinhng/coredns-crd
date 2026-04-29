# coredns-crd Helm chart

CoreDNS as cluster DNS, with DNS-as-code via the DNSSlice CRD.

## Install

```
helm install coredns-crd \
  oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  --version 0.1.0 \
  --namespace kube-system \
  --create-namespace \
  --set service.clusterIP=10.96.0.10
```

The CRD `dnsslices.dns.coredns-crd.io` is installed automatically. Helm 3
will not modify it on `helm upgrade` — apply CRD changes manually with
`kubectl apply -f .../crds/dnsslice.yaml`.

## Verifying provenance

```
cosign verify ghcr.io/hbinhng/coredns-crd:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Replacing the cluster's existing DNS

To use this chart as the cluster's `kube-dns` Service, set
`fullnameOverride: kube-dns` and `service.clusterIP` to the IP that
kubelet's `--cluster-dns` flag points at:

```
helm install coredns-crd ... \
  --set fullnameOverride=kube-dns \
  --set service.clusterIP=10.43.0.10
```

Common cluster-DNS IPs:
- kubeadm: 10.96.0.10
- k3s: 10.43.0.10
- minikube: see `kubectl get svc -n kube-system`

## Values

See `values.yaml` for the full set. Common knobs:

| Key | Default | Notes |
|-|-|-|
| `replicaCount` | 2 | Set ≥2 for HA. |
| `image.tag` | `""` (`.Chart.AppVersion`) | Override for testing. |
| `service.clusterIP` | `""` | Required when replacing kube-dns. |
| `leaderElection.enabled` | `true` | Set false only for `replicaCount: 1` dev. |
| `corefile.kubernetes.enabled` | `true` | Set false to skip cluster.local resolution. |
| `podDisruptionBudget.minAvailable` | 1 | DNS uptime guarantee during drains. |
