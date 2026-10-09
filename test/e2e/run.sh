#!/usr/bin/env bash
# The kind e2e (CI only — it needs Docker and kind): the CRD, an Estate and four Deployments in
# a real cluster; the operator built from this checkout, reporting to a console that does not
# exist. Two parts:
#
#   1. What it observes — within one interval, status.running lists the fixtures with their
#      states (a running own service, a crash loop, an undeclared platform image, an attached
#      service), drift names exactly what it should, and Reported is False/Unreachable.
#   2. The audit envelope (docs/audit-envelope.md) against the DEPLOYED operator, as a
#      checklist an auditor can re-run on any estate: the exact RBAC the identity holds, no
#      Secret, no exec, no Service, no HTTPRoute, no port, no admission webhook, nothing else
#      changes when the operator is scaled to zero, no finalizer.
#
# Every check prints one line, [ ok ] or [FAIL]; the run fails at the end if any check failed.
# To re-run part 2 against your own estate, set the four names below and run the functions
# from `envelope` on.
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tq-operator-e2e}"
IMG="tq-operator:e2e" # the overlay (test/e2e/kustomization.yaml) names this image
OPERATOR_NS="${OPERATOR_NS:-tq-operator}"
PLATFORM_NS="${PLATFORM_NS:-operations}"
SERVICES_NS="${SERVICES_NS:-tequila}"
FLUX_NS="${FLUX_NS:-flux-system}"
SA="system:serviceaccount:${OPERATOR_NS}:tq-operator"

FAILED=()

dump() {
    echo "::group::diagnostics"
    kubectl -n "$OPERATOR_NS" get deploy,pods,estates -o wide || true
    kubectl -n "$OPERATOR_NS" logs deploy/tq-operator --tail=200 || true
    kubectl -n "$OPERATOR_NS" get estate e2e -o yaml || true
    kubectl -n "$OPERATOR_NS" get events --sort-by=.lastTimestamp || true
    kubectl -n "$SERVICES_NS" get deploy,pods -o wide || true
    kubectl -n "$SERVICES_NS" get pods -o json | jq -c '.items[] | {name: .metadata.name, phase: .status.phase, containers: .status.containerStatuses}' || true
    echo "::endgroup::"
}
trap dump ERR

fail() { echo "::error title=e2e::$*"; dump; exit 1; }

# check <line> <description> <function> [args…]: runs it; one checklist line either way.
check() {
    local line="$1" desc="$2"
    shift 2
    if "$@" >/dev/null 2>&1; then
        printf '  [ ok ] %-4s %s\n' "$line" "$desc"
    else
        printf '  [FAIL] %-4s %s\n' "$line" "$desc"
        FAILED+=("$line $desc")
    fi
}

################################################################################################
# Part 1 helpers — all read the Estate's status from $estate.

jqc() { jq -e "$1" <<<"$estate" >/dev/null; }
status_never_names_who_attached() { ! grep -q -e "developer@example-org" -e "/home/developer" <<<"$estate"; }

################################################################################################
# Part 2 helpers — the envelope, each a yes/no question to the cluster.

can() { kubectl auth can-i "$@" --as="$SA" 2>/dev/null || true; }
can_not() { [ "$(can "$@")" = no ]; }
can_do() { [ "$(can "$@")" = yes ]; }

# rules <namespace> — every "<group>/<resource>:<verb>" the operator's identity holds there, as
# the API server answers a SelfSubjectRulesReview (what `kubectl auth can-i --list` shows); the
# grants every authenticated identity has (the selfsubject* reviews) removed. Sorted.
# --validate=false: kubectl's client-side validation lists CRDs, which the impersonated identity
# may not — the review is a built-in kind and the API server validates it anyway.
rules_review() {
    kubectl --as="$SA" create --validate=false -o json -f - <<EOF
{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectRulesReview","spec":{"namespace":"$1"}}
EOF
}
rules() {
    rules_review "$1" | jq -r '
        if .status.incomplete then error("incomplete rules review") else . end
        | .status.resourceRules[]
        | select(([.resources[]? | startswith("selfsubject")] | any) | not)
        | . as $r | $r.apiGroups[] as $g | $r.resources[] as $res | $r.verbs[] as $v
        | "\($g)/\($res):\($v)" + (if (($r.resourceNames // []) | length) > 0 then "[" + ($r.resourceNames | join(",")) + "]" else "" end)' \
        | LC_ALL=C sort -u
}
non_resource_verbs_are_get() {
    [ "$(rules_review "$1" | jq -r '[.status.nonResourceRules[].verbs[]] | unique | join(",")')" = get ]
}
glw() { for v in get list watch; do echo "$1:$v"; done; }
expected_rules() { # the documented envelope of rung observe, per namespace
    {
        glw "/nodes" # the one cluster-wide read, visible in every namespace
        case "$1" in
            "$PLATFORM_NS" | "$SERVICES_NS")
                glw "/pods"; glw "apps/deployments"; glw "apps/replicasets"; glw "external-secrets.io/externalsecrets" ;;
            "$FLUX_NS")
                glw "fluxcd.controlplane.io/fluxinstances"; glw "kustomize.toolkit.fluxcd.io/kustomizations"
                glw "source.toolkit.fluxcd.io/gitrepositories"; glw "source.toolkit.fluxcd.io/ocirepositories" ;;
            "$OPERATOR_NS")
                echo "/events:create"; echo "/events:patch"; glw "estate.tequila.dev/estates"
                for v in get patch update; do echo "estate.tequila.dev/estates/status:$v"; done ;;
        esac
    } | LC_ALL=C sort -u
}
rules_equal() { diff <(expected_rules "$1") <(rules "$1"); }

no_webhooks() { [ -z "$(kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations -o name | grep -i tq-operator || true)" ]; }
can_write_own_status() { can_do update estates --subresource=status -n "$OPERATOR_NS" && can_do patch estates --subresource=status -n "$OPERATOR_NS"; }
can_record_events() { can_do create events -n "$OPERATOR_NS" && can_do patch events -n "$OPERATOR_NS"; }
cannot_write_deployments() { for v in create update patch delete; do can_not "$v" deployments -n "$SERVICES_NS" || return 1; done; }
status_owned_through_subresource() {
    kubectl -n "$OPERATOR_NS" get estate e2e -o json --show-managed-fields \
        | jq -e '[.metadata.managedFields[] | select(.manager == "tq-operator")] | length > 0 and all(.subresource == "status")' >/dev/null
}
no_service() { [ -z "$(kubectl -n "$OPERATOR_NS" get svc -o name)" ]; }
httproute_served() { kubectl api-resources --api-group=gateway.networking.k8s.io -o name 2>/dev/null | grep -q '^httproutes'; }
no_httproute() { [ -z "$(kubectl -n "$OPERATOR_NS" get httproutes -o name)" ]; }
podc() { jq -e "$1" <<<"$pod" >/dev/null; }
args_name_two_hosts_and_no_listener() {
    local args
    args="$(kubectl -n "$OPERATOR_NS" get deploy tq-operator -o jsonpath='{.spec.template.spec.containers[0].args[*]}')"
    grep -q -- "--iam-url=" <<<"$args" && grep -q -- "--console-url=" <<<"$args" && ! grep -q -- "--metrics-bind-address" <<<"$args"
}
snapshot() { # every Deployment outside the operator's namespace, every Kustomization, every Estate — spec and metadata, never status
    kubectl get deployments -A -o json | jq -c --arg ns "$OPERATOR_NS" \
        '[.items[] | select(.metadata.namespace != $ns) | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, labels: .metadata.labels, annotations: .metadata.annotations, spec: .spec}]'
    kubectl get kustomizations -A -o json | jq -c \
        '[.items[] | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, annotations: .metadata.annotations, spec: .spec}]'
    kubectl get estates -A -o json | jq -c \
        '[.items[] | {ns: .metadata.namespace, name: .metadata.name, generation: .metadata.generation, finalizers: .metadata.finalizers, spec: .spec}]'
}
snapshots_equal() { [ "$before" = "$after" ]; }
own_service_still_runs() { kubectl -n "$SERVICES_NS" rollout status deploy/fake --timeout=60s; }
no_estate_finalizers() { kubectl get estates -A -o json | jq -e '[.items[].metadata.finalizers // [] | length] | add == 0' >/dev/null; }
no_estate_gates_flux() {
    kubectl get estates -A -o json | jq -e '[.items[].status | has("observedGeneration") or any(.conditions[]?; .type == "Ready")] | any | not' >/dev/null
}

################################################################################################
echo "— the image (the static binary for the Docker daemon's architecture, then the Dockerfile copies it)"
make "dist-$(docker version --format '{{.Server.Arch}}')"
docker build -t "$IMG" .
kind load docker-image "$IMG" --name "$CLUSTER"

echo "— the CRDs first, so the operator's probe finds every kind at start"
kubectl apply -f test/crds/
kubectl apply -k config/crd
kubectl wait --for condition=established --timeout=60s \
    crd/estates.estate.tequila.dev crd/kustomizations.kustomize.toolkit.fluxcd.io crd/externalsecrets.external-secrets.io

echo "— the estate's namespaces, the Estate, the Deployments and the attached Kustomization"
kubectl apply -f test/e2e/namespaces.yaml
kubectl create namespace "$OPERATOR_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f test/e2e/fixtures.yaml

echo "— the operator (restricted Pod Security enforced on its namespace)"
kubectl apply -k test/e2e
kubectl -n "$OPERATOR_NS" rollout status deploy/tq-operator --timeout=180s

################################################################################################
echo
echo "Part 1 — what the operator observes (one interval: 1m; the crash loop needs its first restart)"
deadline=$((SECONDS + 180))
while :; do
    estate="$(kubectl -n "$OPERATOR_NS" get estate e2e -o json)"
    reported="$(jq -r '.status.conditions[]? | select(.type == "Reported") | "\(.status)/\(.reason)"' <<<"$estate")"
    fake_ready="$(jq '[.status.running.workloads[]? | select(.name == "fake")][0].replicas.ready == 1' <<<"$estate")"
    crasher_looping="$(jq '[.status.running.workloads[]? | select(.name == "crasher")][0] | .crashLooping == true and .restarts >= 1' <<<"$estate")"
    if [ "$fake_ready" = true ] && [ "$crasher_looping" = true ] && [ "$reported" = "False/Unreachable" ]; then
        break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
        fail "after 180 s: fake ready: $fake_ready, crasher crash-looping: $crasher_looping, Reported: ${reported:-none}"
    fi
    sleep 5
done

check O1 "Observed is True" jqc '.status.conditions[] | select(.type == "Observed" and .status == "True")'
check O2 "Reported is False/Unreachable (the console does not exist); lastReport says unreachable" \
    jqc '(.status.conditions[] | select(.type == "Reported") | .status == "False" and .reason == "Unreachable") and .status.lastReport.outcome == "unreachable"'
check O3 "the status never gates Flux: no observedGeneration, no Ready condition" \
    jqc '(.status | has("observedGeneration") | not) and ([.status.conditions[] | select(.type == "Ready")] | length == 0)'
check O4 "cluster facts: the Kubernetes version and at least one node" \
    jqc '.status.cluster.nodes.count >= 1 and .status.cluster.kubernetes != ""'
check O5 "tequila/fake: a tenant's own service — listed, no service name, state managed, 1/1" \
    jqc '[.status.running.workloads[] | select(.name == "fake")][0] | .service == null and .state == "managed" and .undeclared == false and .replicas.ready == 1'
check O6 "tequila/crasher: crashLooping with a restart count; named in health; NotReady drift (1 desired, 0 ready)" \
    jqc '([.status.running.workloads[] | select(.name == "crasher")][0] | .crashLooping == true and .restarts >= 1)
         and (.status.health.pods.crashLooping | index("tequila/crasher") != null)
         and ([.status.drift[] | select(.kind == "NotReady" and .subject == "tequila/crasher")] | length == 1)'
check O7 "tequila/ghost-api: a platform image spec.services does not list — undeclared; one Undeclared drift naming the image" \
    jqc '([.status.running.workloads[] | select(.name == "ghost-api")][0] | .undeclared == true and .service == null)
         and ([.status.drift[] | select(.kind == "Undeclared" and .subject == "tequila/ghost-api")] | length == 1)
         and ([.status.drift[] | select(.kind == "Undeclared")][0].observed == "registry.example.com/platform/ghost-api:v1.0.0")'
check O8 "tequila/attached-api: a developer's checkout runs over it — state attached, service attributed, no drift about it" \
    jqc '([.status.running.workloads[] | select(.name == "attached-api")][0] | .state == "attached" and .service == "attached" and .undeclared == false)
         and ([.status.drift[] | select(.subject | test("attached"))] | length == 0)'
check O9 "flux-system/services-attached: suspended and attached in applied.kustomizations" \
    jqc '[.status.applied.kustomizations[] | select(.name == "services-attached")][0] | .suspended == true and .attached == true'
check O10 "drift is exactly NotReady tequila/crasher + Undeclared tequila/ghost-api; Drifted is True/NotReady" \
    jqc '([.status.drift[] | "\(.kind) \(.subject)"] | sort == ["NotReady tequila/crasher", "Undeclared tequila/ghost-api"])
         and (.status.conditions[] | select(.type == "Drifted") | .status == "True" and .reason == "NotReady")'
check O11 "who attached never reaches the status (the annotation's value is dropped before the cache)" status_never_names_who_attached
check O12 "a workload carries only the schema's keys" \
    jqc '[.status.running.workloads[] | keys[]] | unique - ["crashLooping","images","kind","name","namespace","replicas","restarts","service","state","undeclared"] | length == 0'

################################################################################################
echo
echo "Part 2 — the audit envelope against the deployed operator (docs/audit-envelope.md)"
echo "  the identity: $SA"
for ns in "$OPERATOR_NS" "$PLATFORM_NS" "$SERVICES_NS" "$FLUX_NS" default kube-system; do
    check "#1" "in $ns the identity holds exactly the documented rules (kubectl auth can-i --list --as=$SA -n $ns)" rules_equal "$ns"
    if ! rules_equal "$ns" >/dev/null 2>&1; then
        echo "         expected (<) / actual (>):"
        diff <(expected_rules "$ns") <(rules "$ns") | sed 's/^/         /' || true
    fi
    check "#1" "in $ns the non-resource grants are get only (discovery, version, health — what every identity has)" non_resource_verbs_are_get "$ns"
done
check "#1" "cannot list Pods outside its namespaces" can_not list pods -n default
check "#2" "cannot get Secrets in any namespace" can_not get secrets --all-namespaces
check "#2" "cannot list ConfigMaps in the services namespace" can_not list configmaps -n "$SERVICES_NS"
check "#2" "cannot exec into Pods" can_not create pods --subresource=exec -n "$SERVICES_NS"
check "#2" "cannot port-forward to Pods" can_not create pods --subresource=portforward -n "$SERVICES_NS"
check "#2" "cannot read Pod logs" can_not get pods --subresource=log -n "$SERVICES_NS"
check "#2" "no ValidatingWebhookConfiguration or MutatingWebhookConfiguration of the operator" no_webhooks
check "#3" "can update and patch estates/status in its own namespace" can_write_own_status
check "#3" "cannot write an Estate's spec" can_not patch estates -n "$OPERATOR_NS"
check "#3" "can create and patch Events in its own namespace" can_record_events
check "#3" "cannot create, update, patch or delete Deployments" cannot_write_deployments
check "#3" "the Estate's status is owned by tq-operator, through the status subresource only" status_owned_through_subresource
check "#4" "no Service in the operator's namespace" no_service
if httproute_served; then
    check "#4" "no HTTPRoute in the operator's namespace" no_httproute
else
    printf '  [ ok ] %-4s %s\n' "#4" "no HTTPRoute: the Gateway API is not served in this cluster, so nothing can route to the operator"
fi
pod="$(kubectl -n "$OPERATOR_NS" get pod -l app.kubernetes.io/name=tq-operator -o json | jq '.items[0]')"
check "#4" "the Pod spec declares no ports" podc '[.spec.containers[].ports // [] | length] | add == 0'
check "#4" "the args name the two hosts (--iam-url, --console-url) and no listener (--metrics-bind-address)" args_name_two_hosts_and_no_listener
check "#6" "the Pod runs as non-root, read-only root filesystem, no privilege escalation, all capabilities dropped" \
    podc '.spec.securityContext.runAsNonRoot == true and .spec.containers[0].securityContext.readOnlyRootFilesystem == true
          and .spec.containers[0].securityContext.allowPrivilegeEscalation == false and (.spec.containers[0].securityContext.capabilities.drop | index("ALL") != null)'
check "#6" "the Pod mounts no Secret: volumes are projected, configMap and emptyDir only" \
    podc '[.spec.volumes[] | keys[] | select(. != "name")] | all(. == "projected" or . == "configMap" or . == "emptyDir")'
check "#6" "the operator did not restart during the run" podc '.status.containerStatuses[0].restartCount == 0'

echo "  #8: the operator scaled to zero changes nothing else in the cluster"
before="$(snapshot)"
kubectl -n "$OPERATOR_NS" scale deploy/tq-operator --replicas=0
kubectl -n "$OPERATOR_NS" wait --for=delete pod -l app.kubernetes.io/name=tq-operator --timeout=90s || true
sleep 15
after="$(snapshot)"
check "#8" "Deployments, Kustomizations and Estates (spec, generation, labels, annotations, finalizers) are identical before and after" snapshots_equal
if [ "$before" != "$after" ]; then
    diff <(tr ',' '\n' <<<"$before") <(tr ',' '\n' <<<"$after") | sed 's/^/         /' || true
fi
check "#8" "the tenant's own service kept running without the operator" own_service_still_runs
check "#8" "no Estate carries a finalizer" no_estate_finalizers
check "#8" "no Estate carries observedGeneration or a Ready condition (Flux never waits on it)" no_estate_gates_flux
kubectl -n "$OPERATOR_NS" scale deploy/tq-operator --replicas=1

################################################################################################
echo
if [ "${#FAILED[@]}" -gt 0 ]; then
    printf '%d check(s) failed:\n' "${#FAILED[@]}"
    printf '  - %s\n' "${FAILED[@]}"
    dump
    exit 1
fi
echo "PASS — every check held"
