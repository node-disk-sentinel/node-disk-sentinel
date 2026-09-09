ROOT := $(realpath $(dir $(realpath $(firstword $(MAKEFILE_LIST))))/..)
BIN_DIR := $(ROOT)/bin

KIND := $(BIN_DIR)/kind
KIND_VERSION := v0.27.0

HELM := $(BIN_DIR)/helm
HELM_VERSION := v3.17.1

CLUSTER_NAME ?= nds-dev
MK_HOST_ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
ARCH ?= $(MK_HOST_ARCH)
export ARCH

VERSION ?= $(shell git describe --tags --always --match 'v*' --abbrev=12 2>/dev/null || echo "dev")
IMAGE_REPO ?= ghcr.io/node-disk-sentinel/node-disk-sentinel
IMAGE_TAG ?= $(VERSION)-$(ARCH)

.PHONY: help cluster-up cluster-down build-image load-image deploy undeploy sync-devices monitoring-up monitoring-down status logs

help:
	@echo "Local Development & Testing Environment for node-disk-sentinel"
	@echo ""
	@echo "Usage: make <target>  (from ./hack) or make -C hack <target> (from repo root)"
	@echo ""
	@echo "Available targets:"
	@echo "  cluster-up        Create a local Kind cluster with disk metadata mounts"
	@echo "  cluster-down      Delete the local Kind cluster"
	@echo "  build-image       Build local Docker image ($(IMAGE_REPO):$(IMAGE_TAG))"
	@echo "  load-image        Load Docker image into Kind cluster"
	@echo "  deploy            Deploy CRDs and node-disk-sentinel via Helm into the cluster"
	@echo "  undeploy          Uninstall node-disk-sentinel and CRDs"
	@echo "  sync-devices      Sync host block device nodes into the Kind node container"
	@echo "  monitoring-up     Start local Prometheus & Grafana stack and import the smartctl dashboard"
	@echo "  monitoring-down   Stop local Prometheus & Grafana test stack"
	@echo "  status            Show status of cluster pods, CRDs, and metrics"
	@echo "  logs              Follow logs of node-disk-sentinel DaemonSet"

$(BIN_DIR):
	@mkdir -p $@

$(KIND): | $(BIN_DIR)
	@echo "Installing kind $(KIND_VERSION)..."
	GOBIN=$(BIN_DIR) go install sigs.k8s.io/kind@$(KIND_VERSION)

$(HELM): | $(BIN_DIR)
	@echo "Installing helm $(HELM_VERSION)..."
	@curl -fsSL https://get.helm.sh/helm-$(HELM_VERSION)-linux-$(ARCH).tar.gz | tar -xz -C $(BIN_DIR) --strip-components=1 linux-$(ARCH)/helm

cluster-up: $(KIND)
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(CLUSTER_NAME)$$"; then \
		echo "Kind cluster '$(CLUSTER_NAME)' is already running."; \
	else \
		echo "Creating Kind cluster '$(CLUSTER_NAME)'..."; \
		$(KIND) create cluster --name $(CLUSTER_NAME) --config $(ROOT)/hack/kind-config.yaml; \
	fi

cluster-down: $(KIND)
	$(KIND) delete cluster --name $(CLUSTER_NAME)

build-image:
	docker build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(ARCH) -t $(IMAGE_REPO):$(IMAGE_TAG) -f $(ROOT)/build/Dockerfile $(ROOT)

load-image: $(KIND) build-image
	$(KIND) load docker-image $(IMAGE_REPO):$(IMAGE_TAG) --name $(CLUSTER_NAME)

deploy: $(HELM) load-image
	@echo "Deploying CRDs..."
	$(HELM) upgrade --install node-disk-sentinel-crd $(ROOT)/charts/node-disk-sentinel-crd \
		--namespace nds-system --create-namespace
	@echo "Deploying node-disk-sentinel DaemonSet..."
	$(HELM) upgrade --install node-disk-sentinel $(ROOT)/charts/node-disk-sentinel \
		--namespace nds-system \
		--set image.repository=$(IMAGE_REPO) \
		--set image.tag=$(IMAGE_TAG) \
		--set image.pullPolicy=Never
	kubectl rollout status daemonset/node-disk-sentinel --namespace nds-system --timeout=2m

undeploy: $(HELM)
	-$(HELM) uninstall node-disk-sentinel -n nds-system
	-$(HELM) uninstall node-disk-sentinel-crd -n nds-system

sync-devices:
	@echo "Syncing host block devices to Kind node '$(CLUSTER_NAME)-control-plane'..."
	@awk '$$1 ~ /^[0-9]+$$/ {print $$1, $$2, $$4}' /proc/partitions | while read -r major minor dev; do \
		if ! docker exec $(CLUSTER_NAME)-control-plane test -e /dev/$$dev; then \
			echo "  Creating /dev/$$dev (b $$major $$minor)..."; \
			docker exec $(CLUSTER_NAME)-control-plane mknod /dev/$$dev b $$major $$minor; \
		fi; \
	done
	@echo "Restarting node-disk-sentinel pod to trigger immediate discovery..."
	-kubectl delete pod -n nds-system -l app.kubernetes.io/name=node-disk-sentinel --wait=false

monitoring-up:
	docker compose -f $(ROOT)/hack/monitoring/docker-compose.yaml up -d --force-recreate
	@until curl -fsS http://localhost:3000/api/health >/dev/null; do sleep 1; done
	curl -fsSL https://grafana.com/api/dashboards/22604/revisions/latest/download | \
		jq -c '{dashboard: ., inputs: [{name: "DS_PROMETHEUS", type: "datasource", pluginId: "prometheus", value: "Prometheus"}], overwrite: true}' | \
		curl -fsS -X POST http://localhost:3000/api/dashboards/import -H 'Content-Type: application/json' --data-binary @- >/dev/null
	@echo ""
	@echo "Monitoring stack is ready:"
	@echo "  * Grafana:    http://localhost:3000"
	@echo "  * Prometheus: http://localhost:9090"
	@echo "  * Metrics:    http://localhost:8080/metrics"
	@echo ""

monitoring-down:
	docker compose -f $(ROOT)/hack/monitoring/docker-compose.yaml down

status:
	@echo "=== Nodes ==="
	@kubectl get nodes -o wide
	@echo "\n=== Pods (nds-system) ==="
	@kubectl get pods -n nds-system -o wide
	@echo "\n=== PhysicalDisks CRDs ==="
	@kubectl get physicaldisks -A 2>/dev/null || echo "No PhysicalDisks found or CRD not ready."
	@echo "\n=== Local Metrics Probe (localhost:8080) ==="
	@curl -s -m 2 http://localhost:8080/metrics | grep -E "^(smartctl_device_|go_goroutines)" | head -n 10 || echo "Port 8080 not reachable"

logs:
	kubectl -n nds-system logs -l app.kubernetes.io/name=node-disk-sentinel -f
