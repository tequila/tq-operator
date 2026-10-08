# tq-operator — the build. Tools run pinned through `go run <module>@<version>`, so nothing is
# installed and go.mod carries the operator's own dependencies only.

CONTROLLER_TOOLS_VERSION ?= v0.22.0
SETUP_ENVTEST_VERSION    ?= v0.25.2
# The Kubernetes version envtest's API server and etcd come from.
ENVTEST_K8S_VERSION      ?= 1.37.x
GOLANGCI_LINT_VERSION    ?= v2.14.0

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST  ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
GOLANGCI_LINT  ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

LOCALBIN ?= $(CURDIR)/bin
IMG      ?= tq-operator:dev

.PHONY: all
all: manifests test build

## Generated artifacts: deepcopy, the CRD, the RBAC of rung observe, the two published schemas.
.PHONY: manifests
manifests: generate
	$(CONTROLLER_GEN) rbac:roleName=tq-operator-observe-cluster,fileName=role.yaml crd \
		paths="./api/..." paths="./internal/controller/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac/observe
	go run ./hack/schemagen

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run ./...

## Unit tests and the envtest suite (a real API server and etcd; no cluster, no kind).
.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./... -count=1

.PHONY: envtest
envtest:
	@mkdir -p $(LOCALBIN)
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path >/dev/null

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(LOCALBIN)/tq-operator ./cmd

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

## The kind e2e — CI runs it (test/e2e/run.sh); it needs a Docker daemon and kind.
.PHONY: test-e2e
test-e2e:
	IMG=$(IMG) ./test/e2e/run.sh
