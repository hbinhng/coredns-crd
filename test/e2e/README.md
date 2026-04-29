# E2E

Run the same scenarios CI runs, against any current-context cluster.

## Prerequisites

- `kubectl` configured against a target cluster (KinD, k3s, anything).
- `helm` 3.8+.
- The image `coredns-crd:e2e` loaded into the cluster's runtime:

      docker build -t coredns-crd:e2e .
      # KinD:
      kind load docker-image coredns-crd:e2e --name <cluster-name>
      # k3s:
      docker save coredns-crd:e2e | sudo k3s ctr images import -

## Run

    bash test/e2e/run.sh

The script tears down its own helper pods on EXIT (trap). The chart
remains installed at the end so you can inspect state. To reset:

    helm uninstall coredns-crd -n kube-system
    kubectl delete dnsslice --all
    kubectl delete -f deploy/helm/coredns-crd/crds/dnsslice.yaml

## Scenarios

| # | Name | What it asserts |
|-|-|-|
| 1 | Install | Helm rollout succeeds; lease acquired within 60s. |
| 2 | Resolution | A/AAAA/CNAME/TXT/SRV all return expected values. |
| 3 | Metrics | All 8 `coredns_crd_*` series exposed. |
| 4 | Conflict | Loser slice flips `Conflicting=True` and fires `ConflictDetected` Event. |
| 5 | Failover | Killing the leader → new lease holder within 30s; DNS still resolves. |
