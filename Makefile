ROOT := $(realpath $(dir $(realpath $(firstword $(MAKEFILE_LIST)))))
BIN_DIR := $(ROOT)/bin

CONTROLLER_GEN := $(BIN_DIR)/controller-gen
CONTROLLER_GEN_VERSION := v0.17.2

GOLANGCI_LINT := $(BIN_DIR)/golangci-lint
GOLANGCI_LINT_VERSION := v1.64.5
GOLANGCI_LINT_TIMEOUT ?= 5m

MK_HOST_ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
ARCH ?= $(MK_HOST_ARCH)
export ARCH

VERSION ?= $(shell git describe --tags --always --match 'v*' --abbrev=12 2>/dev/null || echo "dev")
IMAGE_REPO ?= ghcr.io/node-disk-sentinel/node-disk-sentinel
IMAGE_TAG ?= $(VERSION)-$(ARCH)

LDFLAGS := -s -w -X main.version=$(VERSION)

# Output formatting
ifdef CI
  BOLD  :=
  CYAN  :=
  RED   :=
  RESET :=
else
  BOLD  := \033[1m
  CYAN  := \033[36m
  RED   := \033[31m
  RESET := \033[0m
endif

BANNER = @printf "$(BOLD)$(CYAN)[target: $@]$(RESET)\n"
ERROR  = printf "$(BOLD)$(RED)ERROR: %s$(RESET)\n"

.DEFAULT_GOAL := default

.PHONY: default build test validate validate-ci fix generate verify-api package save ci clean

# Local default: generate CRDs, lint, run unit tests, and compile local binary
default: generate validate test build

# Full CI target: ensure CRDs/DeepCopy are up-to-date, code is clean, tests pass, and binary compiles
ci: verify-api validate-ci test build

# ---- Directories ----
$(BIN_DIR):
	@mkdir -p $@

# ---- Build Binary ----
build: | $(BIN_DIR)
	$(BANNER)
	CGO_ENABLED=0 GOARCH=$(ARCH) go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/node-disk-sentinel ./cmd/node-disk-sentinel

# ---- Tests ----
test:
	$(BANNER)
	go test -v -race -cover ./...

# ---- Code Quality / Linting ----
validate: $(GOLANGCI_LINT)
	$(BANNER)
	$(GOLANGCI_LINT) run --timeout $(GOLANGCI_LINT_TIMEOUT) ./...

# ---- CI Validation (checks code formatting & git clean state) ----
validate-ci: validate
	$(BANNER)
	@git diff --exit-code || ($(ERROR) "Git repository is dirty after validation!" && exit 1)

# ---- Auto-fix code formatting ----
fix: $(GOLANGCI_LINT)
	$(BANNER)
	@echo "Formatting Go files ..."
	@go fmt ./...
	@$(GOLANGCI_LINT) run --timeout $(GOLANGCI_LINT_TIMEOUT) --fix ./...

# ---- Generate CRD and DeepCopy ----
# NOTE: controller-gen does not know about the helm.sh/resource-policy annotation,
# so it must be re-injected into every generated CRD file after each run to prevent
# Helm from deleting CRDs (and their data) on chart uninstall.
generate: $(CONTROLLER_GEN)
	$(BANNER)
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate/boilerplate.go.txt" paths="./pkg/apis/..."
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:crdVersions=v1 paths="./pkg/apis/..." output:crd:dir=./charts/node-disk-sentinel-crd/templates
	@for f in $(ROOT)/charts/node-disk-sentinel-crd/templates/*.yaml; do \
		grep -q 'helm.sh/resource-policy' $$f || sed -i '/controller-gen.kubebuilder.io\/version:/a\    helm.sh/resource-policy: keep' $$f; \
	done

# ---- Verify that generated API code and CRDs are up to date ----
verify-api: generate
	$(BANNER)
	@if [ -n "$$(git status --porcelain pkg/apis/ charts/node-disk-sentinel-crd/templates/)" ]; then \
		$(ERROR) "Generated API artifacts (DeepCopy or CRDs) are out of date:"; \
		git status --short pkg/apis/ charts/node-disk-sentinel-crd/templates/; \
		git diff pkg/apis/ charts/node-disk-sentinel-crd/templates/; \
		echo "Please run 'make generate' locally and commit the resulting changes."; \
		exit 1; \
	fi

# ---- Package Docker image ----
package:
	$(BANNER)
	docker build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(ARCH) -t $(IMAGE_REPO):$(IMAGE_TAG) -f $(ROOT)/build/Dockerfile $(ROOT)

# ---- Export Docker image to tar archive ----
save: package | $(BIN_DIR)
	$(BANNER)
	docker save $(IMAGE_REPO):$(IMAGE_TAG) -o $(BIN_DIR)/node-disk-sentinel-$(IMAGE_TAG).tar
	@echo "Saved image archive to $(BIN_DIR)/node-disk-sentinel-$(IMAGE_TAG).tar"

# ---- Clean ----
clean:
	$(BANNER)
	@rm -rf $(BIN_DIR)

# ---- Tool binaries ----
$(CONTROLLER_GEN): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(GOLANGCI_LINT): | $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
