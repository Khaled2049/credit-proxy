SHELL := /bin/sh

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS  := -X github.com/kh1011/creditproxy/pkg/version.Version=$(VERSION) \
            -X github.com/kh1011/creditproxy/pkg/version.Commit=$(COMMIT)

.PHONY: help deps fmt test version tag build-images release \
        run-gateway run-usage run-llmproxy run-ledger \
        docker-up docker-down docker-logs smoke smoke-bdd

help:
	@echo "Available targets:"
	@echo ""
	@echo "  Development"
	@echo "  make deps         - Install/update Go dependencies"
	@echo "  make fmt          - Format Go source files"
	@echo "  make test         - Run all Go tests"
	@echo "  make run-gateway  - Run gateway service locally"
	@echo "  make run-usage    - Run usage service locally"
	@echo "  make run-llmproxy - Run llmproxy service locally"
	@echo "  make run-ledger   - Run ledger service locally"
	@echo ""
	@echo "  Docker"
	@echo "  make docker-up    - Build and start all services with Docker Compose"
	@echo "  make docker-down  - Stop and remove Docker Compose services"
	@echo "  make docker-logs  - Tail Docker Compose logs"
	@echo ""
	@echo "  Testing"
	@echo "  make smoke        - Run curl smoke flow (requires running stack)"
	@echo "  make smoke-bdd    - Run BDD smoke tests (requires running stack)"
	@echo ""
	@echo "  Release"
	@echo "  make version      - Print current version and commit"
	@echo "  make tag TAG=v1.2.3       - Create annotated git tag"
	@echo "  make build-images         - Build all 4 Docker images at current version"
	@echo "  make release TAG=v1.2.3   - tag + build-images in sequence"

# ── Development ───────────────────────────────────────────────────────────────

deps:
	go mod tidy

fmt:
	gofmt -w $$(go list -f '{{.Dir}}' ./... | tr '\n' ' ')

test:
	go test ./...

run-gateway:
	go run -ldflags "$(LDFLAGS)" ./cmd/gateway

run-usage:
	go run -ldflags "$(LDFLAGS)" ./cmd/usage

run-llmproxy:
	go run -ldflags "$(LDFLAGS)" ./cmd/llmproxy

run-ledger:
	go run -ldflags "$(LDFLAGS)" ./cmd/ledger

# ── Docker ────────────────────────────────────────────────────────────────────

docker-up:
	docker compose up --build -d

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f --tail=200

# ── Testing ───────────────────────────────────────────────────────────────────

smoke-bdd:
	go test -v -tags smoke ./tests/smoke/

smoke:
	curl -sS -X POST http://localhost:8080/v1/generate \
	  -H 'content-type: application/json' \
	  -H 'Idempotency-Key: smoke-1' \
	  -d '{"user_id":"u1","prompt":"Write a dramatic chapter opening.","max_output_tokens":120}' && echo

# ── Release ───────────────────────────────────────────────────────────────────

version:
	@echo "Version:  $(VERSION)"
	@echo "Commit:   $(COMMIT)"

tag:
	@test -n "$(TAG)" || (echo "Usage: make tag TAG=v1.2.3" && exit 1)
	git tag -a $(TAG) -m "Release $(TAG)"
	@echo "Tagged $(TAG). Push with: git push origin $(TAG)"

build-images:
	docker build \
	  --build-arg SERVICE=gateway \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg GIT_COMMIT=$(COMMIT) \
	  -t creditproxy-gateway:$(VERSION) \
	  -t creditproxy-gateway:latest .
	docker build \
	  --build-arg SERVICE=usage \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg GIT_COMMIT=$(COMMIT) \
	  -t creditproxy-usage:$(VERSION) \
	  -t creditproxy-usage:latest .
	docker build \
	  --build-arg SERVICE=llmproxy \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg GIT_COMMIT=$(COMMIT) \
	  -t creditproxy-llmproxy:$(VERSION) \
	  -t creditproxy-llmproxy:latest .
	docker build \
	  --build-arg SERVICE=ledger \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg GIT_COMMIT=$(COMMIT) \
	  -t creditproxy-ledger:$(VERSION) \
	  -t creditproxy-ledger:latest .
	@echo ""
	@echo "Built images at $(VERSION):"
	@docker images --format "  {{.Repository}}:{{.Tag}}" | grep "creditproxy-" | grep -v "latest" | sort

release:
	@test -n "$(TAG)" || (echo "Usage: make release TAG=v1.2.3" && exit 1)
	$(MAKE) tag TAG=$(TAG)
	$(MAKE) build-images VERSION=$(TAG)
