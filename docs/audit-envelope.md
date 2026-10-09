# The audit envelope — a checklist your auditor runs

`tq-operator` runs in your cluster. This page shows how to verify what it can do, using your
own `kubectl` and without trusting Tequila. It covers each line of the operator's audit
envelope — the twelve promises below — at rung **`observe`**. That is the only rung that exists
in v0.x.

Set these once:

```bash
SA=system:serviceaccount:tq-operator:tq-operator   # the operator's identity
ENV=prod                                            # the environment this cluster runs
PLATFORM_NS=operations                              # environments.<env>: the platform namespace
SERVICES_NS=tequila                                 # …and the services namespace
```

Each line below gives the requirement, how the operator meets it, the commands to run, and the
answer you should get. **Status** says whether the line is met at v0.1.0 or still pending.

The same lines run as a checklist on every change, against the operator deployed into a kind
cluster: [`test/e2e/run.sh`](../test/e2e/run.sh), part 2 — one `[ ok ]` or `[FAIL]` line per
check. Its commands are the ones below; `OPERATOR_NS`, `PLATFORM_NS`, `SERVICES_NS` and
`FLUX_NS` point it at your estate.

---

## 1. RBAC is read-only on named kinds — met

The operator reads Deployments, ReplicaSets, Pods and ExternalSecrets in the two estate
namespaces. It reads Flux's Kustomizations, GitRepositories, OCIRepositories and FluxInstances
in `flux-system`, Nodes cluster-wide, and Estates in its own namespace. It reads nothing else.

```bash
kubectl get clusterrole,role -A -l app.kubernetes.io/name=tq-operator
kubectl describe clusterrole tq-operator-observe-cluster
kubectl describe role -n tq-operator tq-operator-self
kubectl describe role -n "$PLATFORM_NS" tq-operator-observe
kubectl describe role -n "$SERVICES_NS" tq-operator-observe
kubectl describe role -n flux-system tq-operator-observe-flux
for ns in tq-operator "$PLATFORM_NS" "$SERVICES_NS" flux-system default kube-system; do
  echo "== $ns"; kubectl auth can-i --list --as="$SA" -n "$ns"
done
```

**Expect:**

- one ClusterRole (`nodes`: get, list, watch);
- four Roles: `tq-operator-self`, `tq-operator-observe` twice (once per estate namespace) and
  `tq-operator-observe-flux`;
- in `default` and `kube-system`, nothing beyond what every authenticated identity has
  (`selfsubjectreviews`, the discovery endpoints).

The e2e compares the `--list` answer of every namespace, rule for rule, with this list: in the
estate namespaces `pods`, `deployments`, `replicasets`, `externalsecrets` (get, list, watch); in
`flux-system` `kustomizations`, `gitrepositories`, `ocirepositories`, `fluxinstances` (get,
list, watch); in `tq-operator` `estates` (get, list, watch), `estates/status` (get, update,
patch) and `events` (create, patch); everywhere `nodes` (get, list, watch); and no non-resource
verb but `get`.

## 2. No Secrets, no exec, no admission webhook — met

```bash
kubectl auth can-i get secrets --all-namespaces --as="$SA"                         # no
kubectl auth can-i list configmaps --all-namespaces --as="$SA"                     # no
kubectl auth can-i create pods --subresource=exec -n "$SERVICES_NS" --as="$SA"     # no
kubectl auth can-i create pods --subresource=portforward -n "$SERVICES_NS" --as="$SA"  # no
kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations           # none of the operator's
kubectl -n tq-operator get deploy tq-operator \
  -o jsonpath='{.spec.template.spec.containers[*].ports}{"\n"}'                     # empty: no port
kubectl -n tq-operator get svc                                                      # No resources found
```

The image is distroless and has no shell, so `kubectl exec` into it does nothing. The evidence
that nothing listens is therefore the Pod spec (no `ports:`) and the source code:

- the manager's metrics, health-probe and pprof servers are disabled;
- its webhook server refuses to start (`cmd/main.go`, `managerOptions`, held by
  `cmd/main_test.go`).

## 3. The operator's only write is its own status — met

```bash
kubectl auth can-i patch estates --subresource=status -n tq-operator --as="$SA"  # yes
kubectl auth can-i patch estates -n tq-operator --as="$SA"                       # no: spec is Flux's
kubectl auth can-i create events -n tq-operator --as="$SA"                       # yes
for verb in create update patch delete; do
  kubectl auth can-i "$verb" deployments -n "$SERVICES_NS" --as="$SA"            # no ×4
done
kubectl get events -n tq-operator --field-selector involvedObject.kind=Estate
kubectl -n tq-operator get estate "$ENV" -o yaml --show-managed-fields | grep -E 'manager:|subresource:'
```

**Expect:** the managed fields show `spec` owned by Flux (`kustomize-controller`), and
`status` owned by `tq-operator` with `subresource: status`.

## 4. Egress-only HTTPS to Tequila's two hosts; no inbound port — met (network: see note)

```bash
kubectl -n tq-operator get networkpolicy tq-operator -o yaml
kubectl get svc,httproute -n tq-operator                                    # none
kubectl -n tq-operator get deploy tq-operator \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}'
```

**Expect:**

- the args name exactly two hosts, `--iam-url=https://id.tequila.dev` and
  `--console-url=https://console.tequila.dev`;
- the args do not include `--allow-any-host`;
- the policy allows no ingress, and allows egress only to DNS, TCP 443 on non-RFC 1918
  addresses, and Alloy's OTLP port.

The operator refuses to start if either URL is not `https://` under `tequila.dev`. It takes no
proxy from its environment and follows no redirect.

On a CNI that enforces NetworkPolicy, run this from another namespace. It finds nothing to
connect to:

```bash
POD_IP=$(kubectl -n tq-operator get pod -l app.kubernetes.io/name=tq-operator -o jsonpath='{.items[0].status.podIP}')
kubectl run envelope-probe --rm -i --restart=Never --image=busybox:1.37 -n default -- \
  sh -c "for p in 80 443 8080 8081 8443 9443; do nc -z -w 3 $POD_IP \$p && echo open \$p; done; echo done"
```

**Expect:** `done`, with no `open` line.

> **Note:** a NetworkPolicy is enforced only where the CNI enforces it. Check yours, for
> example the VPC CNI's network policy mode on EKS or Cilium's policy enforcement. The
> operator's own posture (no socket, two pinned hosts) does not depend on the CNI.

## 5. Transparency: the last report is visible in your cluster — met

`Estate.status` is the report. The report's `cluster`, `applied`, `running`, `externalSecrets`,
`drift`, `preflight` and `health` blocks are the status blocks of the same name. The rest of
the report:

| Field | Comes from |
|---|---|
| `declared` | `spec`, echoed (product, source, renderers, platform pins, services) |
| `estate` | the cluster's identity (`estate:<account>/<env>`) |
| `operator` | the operator's version, image and capabilities |
| `reportedAt` | when the report was assembled |

```bash
kubectl -n tq-operator get estate "$ENV" -o yaml
kubectl -n tq-operator get estate "$ENV" -o json \
  | jq '.status | {cluster, applied, running, externalSecrets, drift, preflight, health, lastReport}'
VERSION=$(kubectl -n tq-operator get deploy tq-operator -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's/.*://')
curl -fsSLO "https://github.com/tequila/tq-operator/releases/download/$VERSION/report.estate.v1.json"
jq '.properties | keys' report.estate.v1.json
```

The schema is closed: `additionalProperties: false` on every object, every key required, every
string at most 512 characters. The console refuses a report outside it (`422`).

The schema has no place for any of these, so a report never carries them:

- a node name;
- a Pod name (workloads are named `<namespace>/<Deployment>`);
- an environment variable or an annotation;
- a label;
- a log line;
- an event message (only Flux's `reason` word is carried);
- anything from a Secret or a ConfigMap.

When `operator.report.ownServices` is `false` in `tequila.yaml`, your own services are left out
of `running`, `drift` and the crash-loop names entirely. A workload running a platform image that
your `tequila.yaml` does not declare is not one of your own services: it stays in the report,
flagged `undeclared`, with one `Undeclared` drift naming its image.

Two more facts the report carries about a workload, and what it never carries with them:

- `state: attached` says a developer's inner loop runs a checkout over the workload — the Flux
  Kustomization is suspended and annotated. The operator reads the annotation's presence; the
  value (who attached, from where, since when) is dropped before the object enters its cache and
  cannot appear in the status or the report.
- `crashLooping` and `restarts` come from the Pods' container statuses — the waiting reason, the
  terminated state's exit code and reason, and the restart counts; never a log line, never a
  termination message, never a Pod name.

## 6. Supply chain and runtime posture — partly met

Met at v0.1.0:

```bash
kubectl get ns tq-operator -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}{"\n"}'   # restricted
kubectl -n tq-operator get deploy tq-operator -o json | jq '.spec.template.spec |
  {securityContext, container: .containers[0].securityContext, resources: .containers[0].resources,
   volumes: [.volumes[] | keys[] | select(. != "name")]}'
```

**Expect:**

- `runAsNonRoot: true`, user 65532, `seccompProfile: RuntimeDefault`;
- `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]`;
- limits of 200m CPU and 192Mi memory;
- volumes of type `projected`, `configMap` and `emptyDir` only — no Secret.

The image is a single static binary (`CGO_ENABLED=0`) on `gcr.io/distroless/static-debian12:nonroot`,
for linux/amd64 and linux/arm64. Its base image is pinned by digest in the `Dockerfile`, which only
copies the binary; the same binaries are attached to the GitHub Release with `SHA256SUMS`.

**Pending (a later release, with the signed-image catalogue):**

- the image signed with the platform's KMS key and an RFC 3161 timestamp;
- SLSA provenance and an SPDX SBOM;
- the render pinning the image by digest.

Until then, the image is pinned by tag and `signature` reads `unverified` for every image in the
report, the operator's own included. When signing ships:

```bash
cosign verify --key tequila-release.pub --insecure-ignore-tlog=true --use-signed-timestamps \
  --timestamp-certificate-chain tsa-chain.pem registry.tequila.dev/platform/tq-operator@sha256:…
cosign verify-attestation --key tequila-release.pub --type slsaprovenance1 registry.tequila.dev/platform/tq-operator@sha256:…
```

## 7. You control it — met

The operator is yours to switch off and to unbind, from your own repository and cluster:

- **The off switch:** set `environments.<env>.operator.enabled: false` in `tequila.yaml`. The
  next render removes the unit.
- **Status in-cluster only:** set `operator.report.enabled: false`. The Estate's `Reported`
  condition then reads `False/Disabled`.
- **Unbind the cluster:** run `tq estate:identity:revoke --env <env>`. IAM refuses the next
  exchange (`invalid_grant`), and `Reported` reads `False/Unbound` within one interval.

## 8. Failure mode: nothing breaks — met

```bash
snapshot() {   # every Deployment outside the operator's namespace, every Kustomization, every Estate — spec and metadata
  kubectl get deployments -A -o json | jq -c '[.items[] | select(.metadata.namespace != "tq-operator") | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, spec}]'
  kubectl get kustomizations -A -o json | jq -c '[.items[] | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, annotations: .metadata.annotations, spec}]'
  kubectl get estates -A -o json | jq -c '[.items[] | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, finalizers: .metadata.finalizers, spec}]'
}
before=$(snapshot)
kubectl -n tq-operator scale deploy tq-operator --replicas=0      # your workloads are untouched
sleep 15; after=$(snapshot); [ "$before" = "$after" ] && echo identical
kubectl get estate -A -o jsonpath='{range .items[*]}{.metadata.name}{" finalizers="}{.metadata.finalizers}{"\n"}{end}'   # none
kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations   # none of the operator's
kubectl get estate -A -o json | jq '[.items[].status | has("observedGeneration") or any(.conditions[]?; .type == "Ready")] | any'   # false
kubectl -n tq-operator scale deploy tq-operator --replicas=1
```

**The Estate never holds your deploys:** Flux applies the `Estate` with the rest of the platform
estate, and its health check would wait on a custom resource whose top-level
`status.observedGeneration` lags `metadata.generation`, or whose `Ready` condition is `False`.
The `Estate`'s status has neither. Its conditions are `Observed`, `Reported` and `Drifted`, each
with its own `observedGeneration`. An operator that is down, switched off or unbound delays
nothing that comes after it.

**When the console or IAM is down:**

- the status is still written;
- `Reported` reads `False/Unreachable`, with the next attempt's time;
- retries back off from 1 minute to 1 hour.

**When the cluster is unbound:** retries come every 5 minutes for the first hour after start
(an operator usually starts before its binding is registered), then every hour.

**When the operator stalls:** it exits non-zero and restarts. You see this as a restart count,
never as an open port.

## 9. Inspectable source — met

The source is at https://github.com/tequila/tq-operator. Once provenance ships (line 6), it
names the exact commit each image was built from. An external penetration test is planned.

## 10. It never reads a Secret's value — met

Line 2's `can-i` checks show this. The Pod mounts no Secret: its credential is a projected
ServiceAccount token whose audience is IAM, which the kubelet issues and rotates.

```bash
kubectl -n tq-operator get deploy tq-operator -o json | jq '.spec.template.spec.volumes'
```

## 11. It never takes orders from Tequila — met

The operator reads three things from Tequila's answers and nothing else (see
`internal/report/client.go`):

- `access_token` and `expires_in` from IAM;
- an error code from IAM;
- an error code from the console.

Nothing in any answer is applied or written to your cluster. The only object the operator writes
is its own `Estate.status`, built from what it read in your cluster. Line 3's managed fields
confirm it.

## 12. It never renders or applies hidden state — met

Lines 1 and 3 show this: the operator has no `create`, `update` or `delete` on any workload
kind. Its desired state is `Estate.spec`, which your render writes and Flux applies.

```bash
kubectl -n tq-operator get estate "$ENV" -o jsonpath='{.spec}' | jq .
```
