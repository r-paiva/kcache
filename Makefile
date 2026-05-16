# SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
#
# SPDX-License-Identifier: Apache-2.0

BINARY      := kcache
METRICS     := http://localhost:9090/metrics

# Pass extra flags to the binary, e.g. make run FLAGS="-stats 5s"
FLAGS    :=

# Deploy variables — override as needed, e.g. make deploy-cilium IMAGE_TAG=v0.2
CHART         := charts/kcache-demo
RELEASE       := demo
NAMESPACE     := default
IMAGE_NAME    ?= kcache
IMAGE_TAG     ?= dev
CLUSTERS      := flannel cilium calico
MONITORING_NS := monitoring

.PHONY: all build generate test test-v test-race fmt vet lint clean \
        run run-debug run-stats trace metrics metrics-watch deps tidy \
        cluster-create-all cluster-create-flannel cluster-create-cilium cluster-create-calico \
        cluster-delete-all cluster-delete-flannel cluster-delete-cilium cluster-delete-calico \
        cluster-reset-all cluster-reset-flannel cluster-reset-cilium cluster-reset-calico \
        crd-all crd-flannel crd-cilium crd-calico \
        deploy-all deploy-flannel deploy-cilium deploy-calico \
        load-all load-flannel load-cilium load-calico \
        reload-all reload-flannel reload-cilium reload-calico \
        monitoring-all monitoring-flannel monitoring-cilium monitoring-calico \
        test-all test-flannel test-cilium test-calico \
        test-operator-all test-operator-flannel test-operator-cilium test-operator-calico \
        delete-all delete-flannel delete-cilium delete-calico \
        grafana-flannel grafana-cilium grafana-calico \
        image help

# ── Build ─────────────────────────────────────────────────────────────────────

all: generate build

generate:
	go generate ./...

build:
	go build -o $(BINARY) .

test:
	go test ./internal/...

test-v:
	go test -v ./internal/...

test-race:
	go test -race ./internal/...

fmt:
	gofmt -w -s .

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || \
		{ echo "golangci-lint not found — install from https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run ./...

tidy:
	go mod tidy

run: build
	sudo ./$(BINARY) $(FLAGS)

run-debug: build
	sudo ./$(BINARY) -log-level debug $(FLAGS)

run-stats: build
	sudo ./$(BINARY) -stats 5s $(FLAGS)

trace:
	sudo cat /sys/kernel/debug/tracing/trace_pipe

metrics:
	@curl -sf $(METRICS) | grep -E '^(kcache_|#)' || \
		echo "metrics endpoint not reachable — is kcache running?"

metrics-watch:
	watch -n2 "curl -sf $(METRICS) | grep -E '^kcache_' | grep -v '^#'"

deps:
	sudo apt-get install -y clang llvm libbpf-dev linux-libc-dev linux-headers-$$(uname -r)

clean:
	rm -f $(BINARY)
	rm -f kcache_bpfel.go kcache_bpfeb.go kcache_bpfel.o kcache_bpfeb.o

# ── Image ─────────────────────────────────────────────────────────────────────

image:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

# ── Deploy helpers ────────────────────────────────────────────────────────────

define push-image
	minikube ssh --profile $(1) "docker rmi -f $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true"
	minikube image load $(IMAGE_NAME):$(IMAGE_TAG) --profile $(1)
endef

define deploy
	$(call push-image,$(1))
	kubectl --context=$(1) apply -f $(CHART)/crds/
	helm upgrade --install $(RELEASE) $(CHART) \
	  --kube-context=$(1) \
	  --namespace=$(NAMESPACE) \
	  --create-namespace \
	  --set kcache.image.tag=$(IMAGE_TAG) \
	  --wait
	kubectl --context=$(1) rollout restart daemonset \
	  -n $(NAMESPACE) -l app.kubernetes.io/component=kcache 2>/dev/null || true
endef

define load
	$(call push-image,$(1))
	kubectl --context=$(1) rollout restart daemonset \
	  -n $(NAMESPACE) -l app.kubernetes.io/component=kcache 2>/dev/null || true
endef

define reload
	$(call load,$(1))
	kubectl --context=$(1) rollout status daemonset \
	  $$(kubectl --context=$(1) get daemonset -n $(NAMESPACE) \
	      -l app.kubernetes.io/component=kcache \
	      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) \
	  -n $(NAMESPACE) --timeout=90s
endef

define monitoring
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
	helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
	helm repo update
	helm upgrade --install prometheus prometheus-community/prometheus \
	  --kube-context=$(1) \
	  -f monitoring/prometheus-values.yaml \
	  --namespace=$(MONITORING_NS) --create-namespace --wait
	helm upgrade --install grafana grafana/grafana \
	  --kube-context=$(1) \
	  -f monitoring/grafana-values.yaml \
	  --namespace=$(MONITORING_NS) --create-namespace --wait
endef

# ── Cluster lifecycle ─────────────────────────────────────────────────────────

MINIKUBE_MEMORY ?= 4096
MINIKUBE_CPUS   ?= 2

cluster-create-flannel:
	minikube start --profile flannel --cni=flannel \
	  --memory=$(MINIKUBE_MEMORY) --cpus=$(MINIKUBE_CPUS)

cluster-create-cilium:
	minikube start --profile cilium --cni=cilium \
	  --memory=$(MINIKUBE_MEMORY) --cpus=$(MINIKUBE_CPUS)

cluster-create-calico:
	minikube start --profile calico --cni=calico \
	  --memory=$(MINIKUBE_MEMORY) --cpus=$(MINIKUBE_CPUS)

cluster-create-all: cluster-create-flannel cluster-create-cilium cluster-create-calico

cluster-delete-flannel:
	minikube delete --profile flannel

cluster-delete-cilium:
	minikube delete --profile cilium

cluster-delete-calico:
	minikube delete --profile calico

cluster-delete-all: cluster-delete-flannel cluster-delete-cilium cluster-delete-calico

# Delete and recreate a cluster, then deploy.
cluster-reset-flannel: cluster-delete-flannel cluster-create-flannel deploy-flannel
cluster-reset-cilium:  cluster-delete-cilium  cluster-create-cilium  deploy-cilium
cluster-reset-calico:  cluster-delete-calico  cluster-create-calico  deploy-calico
cluster-reset-all: cluster-delete-all cluster-create-all deploy-all

# ── CRD ──────────────────────────────────────────────────────────────────────
# Helm only installs CRDs on first install, not on upgrade. Use these targets
# to apply the latest CRD schema to an existing cluster.

.PHONY: crd-all crd-flannel crd-cilium crd-calico

crd-all: $(addprefix crd-,$(CLUSTERS))

crd-flannel:
	kubectl --context=flannel apply -f $(CHART)/crds/

crd-cilium:
	kubectl --context=cilium apply -f $(CHART)/crds/

crd-calico:
	kubectl --context=calico apply -f $(CHART)/crds/

# ── Deploy ────────────────────────────────────────────────────────────────────

deploy-all: $(addprefix deploy-,$(CLUSTERS))

deploy-flannel:
	$(call deploy,flannel)

deploy-cilium:
	$(call deploy,cilium)

deploy-calico:
	$(call deploy,calico)

# ── Image load ────────────────────────────────────────────────────────────────

load-all: $(addprefix load-,$(CLUSTERS))

load-flannel:
	$(call load,flannel)

load-cilium:
	$(call load,cilium)

load-calico:
	$(call load,calico)

# ── Reload (load + rollout wait) ──────────────────────────────────────────────

reload-all: $(addprefix reload-,$(CLUSTERS))

reload-flannel:
	$(call reload,flannel)

reload-cilium:
	$(call reload,cilium)

reload-calico:
	$(call reload,calico)

# ── Monitoring ────────────────────────────────────────────────────────────────

monitoring-all: $(addprefix monitoring-,$(CLUSTERS))

monitoring-flannel:
	$(call monitoring,flannel)

monitoring-cilium:
	$(call monitoring,cilium)

monitoring-calico:
	$(call monitoring,calico)

# ── Integration tests ─────────────────────────────────────────────────────────

test-all: $(addprefix test-,$(CLUSTERS))

test-flannel:
	bash tests/test-cache.sh $(NAMESPACE) flannel

test-cilium:
	bash tests/test-cache.sh $(NAMESPACE) cilium

test-calico:
	bash tests/test-cache.sh $(NAMESPACE) calico

test-operator-all: $(addprefix test-operator-,$(CLUSTERS))

test-operator-flannel:
	bash tests/test-operator.sh $(NAMESPACE) flannel

test-operator-cilium:
	bash tests/test-operator.sh $(NAMESPACE) cilium

test-operator-calico:
	bash tests/test-operator.sh $(NAMESPACE) calico

# ── Teardown ──────────────────────────────────────────────────────────────────

delete-all: $(addprefix delete-,$(CLUSTERS))

delete-flannel:
	helm uninstall $(RELEASE) --kube-context=flannel --namespace=$(NAMESPACE) 2>/dev/null || true

delete-cilium:
	helm uninstall $(RELEASE) --kube-context=cilium --namespace=$(NAMESPACE) 2>/dev/null || true

delete-calico:
	helm uninstall $(RELEASE) --kube-context=calico --namespace=$(NAMESPACE) 2>/dev/null || true

# ── Grafana port-forward ──────────────────────────────────────────────────────

grafana-flannel:
	kubectl --context=flannel port-forward -n $(MONITORING_NS) svc/grafana 3000:80

grafana-cilium:
	kubectl --context=cilium port-forward -n $(MONITORING_NS) svc/grafana 3001:80

grafana-calico:
	kubectl --context=calico port-forward -n $(MONITORING_NS) svc/grafana 3002:80

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo "Build:"
	@echo "  make                    generate + build"
	@echo "  make image              docker build"
	@echo "  make run / run-debug    run locally (requires root)"
	@echo ""
	@echo "Cluster (MINIKUBE_MEMORY=$(MINIKUBE_MEMORY) MINIKUBE_CPUS=$(MINIKUBE_CPUS)):"
	@echo "  cluster-create-{flannel,cilium,calico,all}  minikube start"
	@echo "  cluster-delete-{flannel,cilium,calico,all}  minikube delete"
	@echo "  cluster-reset-{flannel,cilium,calico,all}   delete + create + deploy"
	@echo ""
	@echo "Deploy (IMAGE_NAME=$(IMAGE_NAME) IMAGE_TAG=$(IMAGE_TAG)):"
	@echo "  crd-{flannel,cilium,calico,all}        apply CRD (helm upgrade won't update it)"
	@echo "  deploy-{flannel,cilium,calico,all}     helm install/upgrade (applies CRD + restarts DS)"
	@echo "  load-{flannel,cilium,calico,all}       push image + restart DaemonSet"
	@echo "  reload-{flannel,cilium,calico,all}     load + wait for rollout"
	@echo "  monitoring-{flannel,cilium,calico,all} install Prometheus + Grafana"
	@echo "  delete-{flannel,cilium,calico,all}     helm uninstall"
	@echo "  grafana-{flannel,cilium,calico}        port-forward Grafana (3000/3001/3002)"
	@echo ""
	@echo "Test:"
	@echo "  test / test-v / test-race              unit tests"
	@echo "  test-{flannel,cilium,calico,all}       integration tests"
	@echo "  test-operator-{flannel,cilium,calico}  operator lifecycle tests"
