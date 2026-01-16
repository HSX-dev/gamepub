GO       ?= go
SERVICES := gateway account wallet order task
SEMGREP_EXCLUDES := --exclude '*_test.go' --exclude api/gen --exclude security/semgrep/testdata

.PHONY: all proto build test race cover lint gosec semgrep vuln trivy security keys up down

all: lint test security build

proto: ## 生成 gRPC 代码
	buf lint && buf generate

build: ## 编译全部服务到 bin/
	@for s in $(SERVICES); do CGO_ENABLED=0 $(GO) build -trimpath -o bin/$$s ./cmd/$$s || exit 1; done

test:
	$(GO) test ./...
	cd tools/ai-review && $(GO) test ./...

race: ## 需要 CGO（gcc）
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -coverprofile=coverage.out ./... && $(GO) tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

gosec:
	gosec -severity medium -confidence medium -exclude-generated -exclude-dir=security ./...

semgrep:
	semgrep --test --config security/semgrep/rules security/semgrep/testdata
	semgrep scan --config security/semgrep/rules --config p/golang $(SEMGREP_EXCLUDES) --error .

vuln:
	govulncheck ./...
	cd tools/ai-review && govulncheck ./...

trivy:
	trivy fs --config security/trivy.yaml .

security: gosec semgrep vuln trivy

keys: ## 生成本地开发密钥
	@test ! -f .env || (echo ".env already exists" && exit 1)
	$(GO) run ./tools/devkeys > .env

up:
	docker compose --env-file .env -f deploy/docker-compose.yml up -d --build

down:
	docker compose --env-file .env -f deploy/docker-compose.yml down -v
