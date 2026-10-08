# tq-operator

The Tequila estate operator. It runs in every hosted Tequila estate (Tequila's own and every
tenant's) and is rung **`observe`** of a ladder: `observe` → `verify` → `gate` → `act`. Each
rung adds one capability, and an estate climbs only as far as its `tequila.yaml` allows. The
principle behind it: *tenants pull content and push status* — Tequila never reaches into a
tenant's cluster; the cluster reports out.

At this rung it does one thing:

1. It **reads** what the cluster runs and what Flux applied.
2. It **compares** that with the `Estate` your render declared from `tequila.yaml`.
3. It **writes** the comparison into `Estate.status`.
4. It **reports** the same payload to `console.tequila.dev`.

It applies nothing, takes no instruction from Tequila, reads no Secret and opens no socket.
**[docs/audit-envelope.md](docs/audit-envelope.md)** is the checklist your auditor runs to
verify that with `kubectl`.

```console
$ kubectl -n tq-operator get estates
NAME   ENVIRONMENT   READY   REPORTED   DRIFTED   AGE
prod   prod          True    True       False     3d
```

## The `Estate` — `estate.tequila.dev/v1alpha1`

Your render emits one `Estate` per environment, into the operator's namespace `tq-operator`.
Flux applies it with the rest of the render. The `spec` is a pure function of `tequila.yaml`.
It carries no commit and no timestamp, so a re-render is byte-identical. The applied commit is
read from Flux in the cluster instead.

`spec` holds:

- `product` and `environment`;
- `source.repository`;
- the renderer pins and the other platform pins;
- the tenant profile;
- the two namespaces;
- every registered service;
- `operator: {capabilities, report: {enabled, ownServices, interval}}`.

See [config/samples/estate_v1alpha1_estate.yaml](config/samples/estate_v1alpha1_estate.yaml).

`status` is the operator's alone, written through the status subresource. Its blocks:

| Block | Holds |
|---|---|
| `cluster` | the Kubernetes version, platform (`eks`, `k3s`, `doks` or `generic`), node count, architectures and kubelet versions — never a node's name |
| `applied` | Flux's source revision; each Kustomization's readiness, applied revision, reason and last transition |
| `running` | every Deployment of the two estate namespaces: replicas, image references, and the digests the kubelet pulled. Also the platform estate's version stamped on its workloads |
| `externalSecrets` | total, ready, and the not-ready ones with ESO's reason |
| `drift` | `RunningBehind`, `AppliedBehind`, `NotReady`, `SecretNotResolvable`, `NoEstate` (`Undeclared` arrives in a later release) |
| `health` | Pods by phase, and crash-looping workloads |
| `conditions` | `Ready`, `Reported` and `Drifted` |
| `lastReport` | when the last report was sent, its outcome and its HTTP status |

The report the console receives is `status` plus a few additions. It adds the declared echo of
`spec` and the estate's identity. It also carries the operator's version, image and capabilities
and the `reportedAt` time. Two schemas are generated from the Go types and published with every
release:

- [schemas/estate.v1alpha1.json](schemas/estate.v1alpha1.json) — the `Estate` (the CRD's own
  OpenAPI schema);
- [schemas/report.estate.v1.json](schemas/report.estate.v1.json) — the report
  `tequila.dev/report/estate/v1`. It is closed (`additionalProperties: false`), every key is
  required, and every string is capped at 512 characters.

## Identity

The operator holds no Tequila secret. Its Pod mounts a projected ServiceAccount token whose
audience is IAM's issuer URL; the kubelet rotates it. Before a report, the operator exchanges
that token at IAM (RFC 8693, `subject_token_type …:jwt`, the console's audience, no client
credentials) for an IAM token `sub: estate:<account>/<env>`. It caches the token until a minute
before expiry, and addresses the report with it:
`PUT https://console.tequila.dev/api/v1/estates/<account>/<env>/report`.

IAM trusts the cluster through the estate's federation binding, which `tq estate:identity`
registers. Deleting the binding stops the reports: `Reported` reads `False/Unbound`.

## Failing harmlessly

| What happens | The operator | `Reported` |
|---|---|---|
| The console accepts the report | re-sends when the payload changes, or once per interval (15m) as a heartbeat | `True/Accepted` |
| The console or IAM is unreachable, or answers 5xx/429 | backs off 1m, 2m, 4m … 1h, honouring `Retry-After`; status is still written | `False/Unreachable` |
| IAM answers `invalid_grant`, or the console answers 401 | retries every 5m for the first hour after start (the binding is usually registered after bootstrap), then hourly | `False/Unbound` |
| Any other refusal (403 `estate_mismatch`, 410, `invalid_target`, 422) | backs off 1m … 1h | `False/Refused`; `lastReport.outcome` is `refused` or `invalid` |
| A kind is not served (no CRD) or not permitted | observes the rest and names the gap; restarts itself once a probe finds the kind readable | `Ready: False/CacheNotSynced` when the gap is a permission |
| No `Estate` is declared | reports the cluster facts with `declared: null` and one `NoEstate` drift | — |
| No reconcile completes in 3 × interval, or the caches do not sync within 5m | exits non-zero; the kubelet restarts it (there is no probe port) | — |

## Flags — what the estate's render sets

| Flag | Default | Meaning |
|---|---|---|
| `--interval` | `15m` | heartbeat and resync; `spec.operator.report.interval` overrides the heartbeat |
| `--iam-url` | — (required) | IAM's issuer URL, e.g. `https://id.tequila.dev` — an https origin under `tequila.dev` |
| `--console-url` | — (required) | `https://console.tequila.dev` |
| `--iam-token-file` | `/var/run/tequila/iam/token` | the projected token (audience = `--iam-url`) |
| `--signing-keys` | `/etc/tq-operator/signing-keys.json` | the rendered public keys (an empty list at this rung) |
| `--allow-any-host` | `false` | accept URLs outside `tequila.dev` (a dev VM's `*.<slug>.test`) |
| `--report-own-services` | `true` | the render's switch; `spec.operator.report.ownServices` must allow it too |
| `--namespace` | `$POD_NAMESPACE`, else `tq-operator` | the operator's own namespace |
| `--platform-namespace` / `--services-namespace` | `operations` / `tequila` | the environment's namespaces — the same values as the `Estate`'s `spec.namespaces` |
| `--flux-namespace` | `flux-system` | where Flux's Kustomizations, sources and FluxInstance live |
| `--image` | `$TQ_OPERATOR_IMAGE` | the operator's own image reference (reported) |
| `--zap-log-level`, `--zap-encoder`, … | | controller-runtime's logging flags |

[config/](config/) holds the kustomize bases the estate's render mirrors:

- `crd/` — the CRD;
- `rbac/observe/` — the Roles generated from the markers, with their bindings;
- `manager/` — the namespace with `pod-security.kubernetes.io/enforce: restricted`, the Pod
  (no port, no probe, no Secret), the signing-keys ConfigMap and the egress-only NetworkPolicy.

## Development

```bash
make manifests   # deepcopy, the CRD, the RBAC of rung observe, both schemas — from the Go types
make test        # unit tests + the envtest suite (setup-envtest fetches an API server and etcd into ./bin)
make lint        # golangci-lint
make build       # CGO_ENABLED=0 binary in ./bin
make test-e2e    # the kind e2e (CI runs it; it needs Docker and kind)
```

The tools run pinned through `go run <module>@<version>`, so `go.mod` carries the operator's
own dependencies only: controller-runtime, client-go, zap and `sigs.k8s.io/yaml`.

The tests that hold the envelope:

| Test | Holds |
|---|---|
| `internal/controller/rbac_golden_test.go` | the generated RBAC equals the hand-written RBAC in `test/golden/rbac-observe.yaml` and grants nothing the envelope forbids |
| `internal/schemagen/schemagen_test.go` | the report schema equals the report contract line for line (`testdata/report.shape`); the committed schemas are fresh |
| `internal/report/allowlist_test.go` | a report with every field of every type filled carries no key outside the schema |
| `internal/report/client_test.go` | the exchange and the door's answers (202, 401, `invalid_grant`, 503 …) and the backoff |
| `internal/controller/estate_controller_test.go` | the reconciler against envtest: status, drift, Events, the report byte-equal to the status, the console going down |
| `cmd/main_test.go` | no listening socket |

## Releases

Every PR is squash-merged, and titles and commits follow Conventional Commits (`pr-semantics`).
`release-please` keeps the release PR current; merging it tags `vX.Y.Z` and publishes the GitHub
Release. In the same workflow run, [release.yaml](.github/workflows/release.yaml):

- builds the two static binaries (`make dist`) and assembles
  `registry.tequila.dev/platform/tq-operator:vX.Y.Z` for linux/amd64 and linux/arm64 from them;
  the job logs in to the registry with its own GitHub OIDC identity, exchanged at Tequila IAM,
  and no stored credential;
- attaches to the GitHub Release: `estates.estate.tequila.dev.yaml` (the CRD),
  `estate.v1alpha1.json`, `report.estate.v1.json`, `rbac.observe.yaml`, the binaries
  `tq-operator-linux-amd64` and `tq-operator-linux-arm64`, and `SHA256SUMS`.

Every workflow runs on GitHub-hosted runners with the default `GITHUB_TOKEN`.

An estate's render vendors those assets per release: the operator's version moves with the
platform estate's version, like every other estate component.
