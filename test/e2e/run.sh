#!/usr/bin/env bash
# The kind e2e (CI only — it needs Docker and kind): the CRD, an Estate and a fake Deployment in
# a real cluster; the operator built from this checkout, reporting to a console that does not
# exist. Done when, within one interval: status.running lists the fake Deployment and the
# Reported condition is False/Unreachable — and the envelope's cheap lines hold
# (docs/audit-envelope.md, lines #1–#4, #6 and #8).
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tq-operator-e2e}"
IMG="tq-operator:e2e" # the overlay (test/e2e/kustomization.yaml) names this image
SA="system:serviceaccount:tq-operator:tq-operator"

dump() {
    echo "::group::diagnostics"
    kubectl -n tq-operator get deploy,pods,estates -o wide || true
    kubectl -n tq-operator logs deploy/tq-operator --tail=200 || true
    kubectl -n tq-operator get estate e2e -o yaml || true
    kubectl -n tq-operator get events --sort-by=.lastTimestamp || true
    echo "::endgroup::"
}
trap dump ERR

fail() { echo "::error title=e2e::$*"; dump; exit 1; }

echo "— the image (the static binary for the Docker daemon's architecture, then the Dockerfile copies it)"
make "dist-$(docker version --format '{{.Server.Arch}}')"
docker build -t "$IMG" .
kind load docker-image "$IMG" --name "$CLUSTER"

echo "— the CRDs first, so the operator's probe finds every kind at start"
kubectl apply -f test/crds/
kubectl apply -k config/crd
kubectl wait --for condition=established --timeout=60s \
    crd/estates.estate.tequila.dev crd/kustomizations.kustomize.toolkit.fluxcd.io crd/externalsecrets.external-secrets.io

echo "— the estate's namespaces, the Estate and a fake Deployment"
kubectl apply -f test/e2e/namespaces.yaml
kubectl create namespace tq-operator --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f test/e2e/fixtures.yaml

echo "— the operator (restricted Pod Security enforced on its namespace)"
kubectl apply -k test/e2e
kubectl -n tq-operator rollout status deploy/tq-operator --timeout=180s

echo "— waiting for the Estate's status (one interval: 1m)"
deadline=$((SECONDS + 90))
while :; do
    estate="$(kubectl -n tq-operator get estate e2e -o json)"
    running="$(jq '[.status.running.workloads[]? | select(.namespace == "tequila" and .name == "fake")] | length' <<<"$estate")"
    reported="$(jq -r '.status.conditions[]? | select(.type == "Reported") | "\(.status)/\(.reason)"' <<<"$estate")"
    if [ "$running" = 1 ] && [ "$reported" = "False/Unreachable" ]; then
        break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
        fail "after 90 s: running has fake: $running, Reported: ${reported:-none}"
    fi
    sleep 5
done
echo "status.running lists tequila/fake; Reported is False/Unreachable"

jq -e '.status.conditions[] | select(.type == "Ready" and .status == "True")' <<<"$estate" >/dev/null \
    || fail "Ready is not True"
jq -e '.status.lastReport.outcome == "unreachable"' <<<"$estate" >/dev/null || fail "lastReport.outcome is not unreachable"
jq -e '.status.cluster.nodes.count >= 1 and .status.cluster.kubernetes != ""' <<<"$estate" >/dev/null || fail "cluster facts missing"
jq -e '[.status.running.workloads[] | select(.name == "fake")][0].service == null' <<<"$estate" >/dev/null \
    || fail "a tenant's own service carries a service name"

echo "— the envelope (docs/audit-envelope.md)"
can() { kubectl auth can-i "$@" --as="$SA"; }
[ "$(can get secrets --all-namespaces || true)" = no ] || fail "#2: the operator can read Secrets"
[ "$(can get configmaps -n tequila || true)" = no ] || fail "#1: the operator can read ConfigMaps"
[ "$(can create pods --subresource=exec -n tequila || true)" = no ] || fail "#2: the operator can exec"
[ "$(can create deployments -n tequila || true)" = no ] || fail "#3: the operator can create workloads"
[ "$(can patch estates -n tq-operator || true)" = no ] || fail "#3: the operator can write an Estate's spec"
[ "$(can patch estates --subresource=status -n tq-operator)" = yes ] || fail "#3: the operator cannot write its status"
[ "$(can list pods -n default || true)" = no ] || fail "#1: the operator reads outside its namespaces"
[ "$(can list deployments -n tequila)" = yes ] || fail "#1: the operator cannot read the services namespace"
[ -z "$(kubectl -n tq-operator get svc -o name)" ] || fail "#4: a Service exists in tq-operator"
pod="$(kubectl -n tq-operator get pod -l app.kubernetes.io/name=tq-operator -o json | jq '.items[0]')"
jq -e '[.spec.containers[].ports // [] | length] | add == 0' <<<"$pod" >/dev/null || fail "#4: the Pod declares a port"
jq -e '.spec.securityContext.runAsNonRoot == true and .spec.containers[0].securityContext.readOnlyRootFilesystem == true' <<<"$pod" >/dev/null \
    || fail "#6: the Pod is not non-root with a read-only root filesystem"
jq -e '.status.containerStatuses[0].restartCount == 0' <<<"$pod" >/dev/null || fail "the operator restarted"
[ -z "$(kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations -o name | grep -i tq-operator || true)" ] \
    || fail "#2: an admission webhook of the operator's exists"

echo "— #8: the operator scaled to zero changes nothing in the estate"
kubectl -n tq-operator scale deploy/tq-operator --replicas=0
kubectl -n tequila rollout status deploy/fake --timeout=60s
[ "$(kubectl -n tq-operator get estate e2e -o jsonpath='{.metadata.finalizers}')" = "" ] || fail "#8: the Estate carries a finalizer"

echo "PASS"
