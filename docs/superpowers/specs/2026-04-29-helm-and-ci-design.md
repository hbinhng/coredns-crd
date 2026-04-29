# Helm Chart + Image CI + Signed Releases

**Sub-project C** in the production-readiness push.
**Date**: 2026-04-29.
**Status**: Approved (autonomous run; user delegated supervision).

## Problem

`coredns-crd` ships today as raw `kubectl apply -f config/...` manifests
plus a hand-built image that gets `docker save | k3s ctr images import`'d
onto one box. That works for the lab and for this session, but it's not
how teams ship cluster DNS:

- Operators expect a Helm chart with values they can tune.
- The image needs to live in a registry anyone can pull from.
- Production deployments require provenance (signatures + attestations)
  to prove the image came from the published source code.
- A CI pipeline gates regressions and produces the artifacts repeatably.

## Goal

Ship the project as a versioned, signed, Helm-installable artifact:

- A Helm chart that templates every existing manifest plus
  PodDisruptionBudget and topology spread.
- A GitHub Actions CI pipeline that runs tests + lint on every PR and
  push.
- A release workflow that publishes a signed image and a signed chart to
  `ghcr.io` on every `v*.*.*` tag.

## Non-goals

- Multi-arch images (amd64 only; arm64 deferred until there's demand).
- SBOM generation beyond what cosign attestations carry.
- Trivy/grype scans in CI (separate concern).
- chart-testing (`ct`) integration tests against KinD (sub-project F
  territory).
- Renovate/dependabot config (separate operational concern).
- An automated Chart.yaml version bumper (simple `sed` in the release
  workflow is enough).

## Design

### Helm chart layout

```
deploy/helm/coredns-crd/
├── Chart.yaml
├── values.yaml
├── README.md
├── templates/
│   ├── _helpers.tpl
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   ├── role-leader.yaml
│   ├── rolebinding-leader.yaml
│   ├── configmap.yaml
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── poddisruptionbudget.yaml
│   └── NOTES.txt
└── crds/
    └── dnsslice.yaml
```

`crds/` is the canonical Helm 3 location for CRDs. Helm installs them
once on first `helm install` and **never** modifies them on `helm
upgrade` or `helm uninstall`. Users who need to upgrade the CRD apply
the new manifest manually — the safe default for cluster-scoped types.

### Values (canonical shape)

```yaml
# Chart-wide
nameOverride: ""
fullnameOverride: ""

replicaCount: 2

image:
  repository: ghcr.io/hbinhng/coredns-crd
  tag: ""               # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent

imagePullSecrets: []

resources:
  requests:
    cpu: 100m
    memory: 70Mi
  limits:
    memory: 200Mi

# Service
service:
  type: ClusterIP
  clusterIP: ""         # explicit when replacing kube-dns (e.g. 10.43.0.10 for k3s)
  ports:
    dns: 53
    dnsTcp: 53
    metrics: 9153
  annotations:
    prometheus.io/port: "9153"
    prometheus.io/scrape: "true"

# Leader election (sub-project A)
leaderElection:
  enabled: true
  namespace: ""         # defaults to release namespace
  leaseName: coredns-crd-leader

# Corefile shape — string template that values.yaml can override piece by piece
corefile:
  zones: [".:53"]
  errors: true
  health:
    enabled: true
    lameduck: 5s
  ready: true
  crd:
    fallthrough: true
  kubernetes:
    enabled: true
    cluster: cluster.local
    pods: insecure
    fallthroughZones: ["in-addr.arpa", "ip6.arpa"]
    ttl: 30
  prometheus:
    listen: ":9153"
  forward:
    upstream: /etc/resolv.conf
    maxConcurrent: 1000
  cache:
    ttl: 30
  loop: true
  reload: true
  loadbalance: true

# PDB (also covers part of sub-project D's "drains take down DNS")
podDisruptionBudget:
  enabled: true
  minAvailable: 1

# Topology
topologySpreadConstraints:
  enabled: true
  maxSkew: 1
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway

# Tolerations + nodeSelector defaults match deploy/coredns-deployment.yaml.
tolerations:
  - key: CriticalAddonsOnly
    operator: Exists
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule

nodeSelector:
  kubernetes.io/os: linux

priorityClassName: system-cluster-critical
```

### Templating the Corefile

`templates/configmap.yaml` walks `.Values.corefile.*` and emits the
Corefile block by block. Each section is conditional — disabling
`kubernetes` removes the `kubernetes ...` plugin entirely; disabling
`prometheus` removes the listen line. This is more flexible than a
single-string `Values.corefile.raw` because operators can tweak one knob
without re-asserting the whole file.

The chart README documents how to drop entirely to a raw Corefile via
`--set-file corefile.raw=path/to/Corefile` for users with bespoke setups.

### CI workflow — `.github/workflows/ci.yml`

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read
  packages: write    # only used in build-image-main; PR builds don't push
  id-token: write    # only used in release.yml — declared here for completeness is fine

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.23' }
      - run: go vet ./...
      - run: go test -race -count=2 -coverprofile=cov.out ./...
      - name: Coverage gate
        run: |
          cov=$(go tool cover -func=cov.out | grep '^total' | awk '{print $3}' | tr -d '%')
          # Fail if any of the well-covered packages drop below 100% after a change.
          for pkg in internal/index internal/leader internal/events; do
            pkg_cov=$(go tool cover -func=cov.out | grep "$pkg" | awk '{print $3}' | tr -d '%' | sort -n | head -1 || true)
            test "${pkg_cov:-100}" = "100.0" || { echo "$pkg dropped below 100%"; exit 1; }
          done

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.23' }
      - uses: azure/setup-helm@v4
      - uses: yannh/kubeconform-action@v1
      - run: helm lint deploy/helm/coredns-crd
      - run: |
          helm template coredns-crd deploy/helm/coredns-crd \
            --set service.clusterIP=10.96.0.10 \
            | kubeconform -strict -summary -

  build-main-image:
    needs: [test, lint]
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: ghcr.io/${{ github.repository }}:main
          labels: |
            org.opencontainers.image.source=https://github.com/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.title=coredns-crd
```

### Release workflow — `.github/workflows/release.yml`

```yaml
name: Release
on:
  push:
    tags: ['v*.*.*']

permissions:
  contents: write    # GitHub Release creation
  packages: write
  id-token: write    # cosign keyless

jobs:
  test:
    # same as ci.yml's test job
    ...

  release:
    needs: test
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      # Image: build, push, sign.
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: |
            ghcr.io/${{ github.repository }}:${{ github.ref_name }}
            ghcr.io/${{ github.repository }}:latest
          labels: |
            org.opencontainers.image.source=https://github.com/${{ github.repository }}
            org.opencontainers.image.revision=${{ github.sha }}
            org.opencontainers.image.version=${{ github.ref_name }}
            org.opencontainers.image.title=coredns-crd
      - uses: sigstore/cosign-installer@v3
      - run: |
          cosign sign --yes \
            ghcr.io/${{ github.repository }}@${{ steps.build.outputs.digest }}

      # Chart: bump version, package, push, sign.
      - uses: azure/setup-helm@v4
      - run: |
          tag=${GITHUB_REF_NAME#v}
          sed -i "s/^version: .*/version: ${tag}/"     deploy/helm/coredns-crd/Chart.yaml
          sed -i "s/^appVersion: .*/appVersion: \"${GITHUB_REF_NAME}\"/" deploy/helm/coredns-crd/Chart.yaml
          helm package deploy/helm/coredns-crd
          helm push coredns-crd-${tag}.tgz oci://ghcr.io/${{ github.repository }}/charts
      - run: |
          tag=${GITHUB_REF_NAME#v}
          cosign sign --yes \
            ghcr.io/${{ github.repository }}/charts/coredns-crd:${tag}

      # GitHub Release with auto-generated notes.
      - uses: softprops/action-gh-release@v2
        with:
          generate_release_notes: true
```

### Tag scheme + version policy

- Releases: `vMAJOR.MINOR.PATCH` semver. While the API is `v1alpha1`,
  releases tag as `v0.MINOR.PATCH`. Bump major to 1 when graduating to
  v1beta1.
- Image tags: `vX.Y.Z` (immutable), `latest` (mutable, follows the most
  recent semver tag), `main` (mutable, HEAD of main, unsigned).
- Chart version mirrors the image tag (sans `v` prefix per Helm
  convention: chart `version: 0.1.0`, `appVersion: "v0.1.0"`).

### Verification (chart README excerpt)

```bash
# Verify image signature (keyless, no public key needed).
cosign verify ghcr.io/hbinhng/coredns-crd:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/hbinhng/coredns-crd/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Install
helm install coredns-crd \
  oci://ghcr.io/hbinhng/coredns-crd/charts/coredns-crd \
  --version 0.1.0 \
  --namespace kube-system \
  --create-namespace \
  --set service.clusterIP=10.96.0.10
```

### Coexistence with `config/`

The existing `config/crd/`, `config/rbac/`, `config/example/`, and
`deploy/coredns-deployment.yaml` stay in the repo as the "raw manifests"
path for users who don't run Helm — and for the existing e2e flow that
sub-projects A and B established. Drift between Helm and raw manifests
is a known risk; `helm template` output is checked manually before
release. A future sub-project can render `config/` from the chart to
eliminate drift, but YAGNI for v1.

### Failure modes

| Mode | Mitigation |
|-|-|
| GitHub Actions secrets/permissions misconfigured | Workflow declares `permissions:` explicitly; `id-token: write` required for cosign keyless. Failures surface as build errors, not silent skips. |
| Chart values mismatch between docs and code | `helm lint` + `helm template | kubeconform` in CI catches structural breakage; semantic mismatches (wrong ConfigMap key etc.) caught only by integration tests in sub-project F. |
| Tag pushed without code on main | The release workflow runs `test` first; if HEAD on the tag fails tests, no artifact is published. |
| Chart name collision in OCI registry | Chart is namespaced under `<repo>/charts/coredns-crd` — no collision with the image at `<repo>:tag`. |
| Cosign keyless infra outage (Sigstore down) | Release fails; retry when Sigstore is back. No fallback (a key-based path would weaken security). |

### Testing

- `helm lint deploy/helm/coredns-crd` — schema sanity.
- `helm template ... | kubeconform -strict` — every rendered manifest
  validates against the live OpenAPI schema.
- `actionlint` in pre-commit (manual) and CI — catches workflow YAML
  errors before push.
- E2E: install the chart from local `deploy/helm/coredns-crd/`
  on the k3s box, validate the same DNS queries as before resolve, and
  metrics endpoint shows zero deltas in `coredns_crd_*` series.

### Out of scope (re-confirmed)

- Multi-arch images (amd64 only).
- SBOMs (cosign attestations cover provenance).
- Trivy/grype scans.
- KinD-based chart e2e in CI (sub-project F).
- Renovate/dependabot.
- Automated Chart.yaml bumping beyond `sed` in the release workflow.

## Acceptance criteria

1. `helm install coredns-crd ./deploy/helm/coredns-crd -n kube-system
   --create-namespace --set service.clusterIP=10.43.0.10` succeeds on
   the k3s box and brings up CoreDNS.
2. `helm lint deploy/helm/coredns-crd` reports zero errors and zero
   warnings.
3. `helm template ... | kubeconform -strict -summary` passes.
4. After install, the same DNS queries from sub-project A's e2e resolve
   correctly and `coredns_crd_*` metrics are reported.
5. `actionlint` (run locally) reports zero errors on `ci.yml` and
   `release.yml`.
6. `go test -race -count=2 ./...` continues to pass.
