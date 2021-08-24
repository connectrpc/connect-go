# See https://tech.davis-hansson.com/p/make/
SHELL := bash
.DELETE_ON_ERROR:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rules
BENCHFLAGS ?= -cpuprofile=cpu.pprof -memprofile=mem.pprof -benchmem -benchtime=30s
GO ?= go

HANDWRITTEN=$(shell find . -type f -name '*.go' | grep -v -e '\.pb\.go$$' -e '\.twirp\.go$$' -e '_string.go$$')
PROTOBUFS=$(shell find . -type f -name '*.proto')

.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "%-30s %s\n", $$1, $$2}'

.PHONY: clean
clean: ## Delete build output
	rm -f bin/{buf,gofmt,goimports,staticcheck,protoc-gen-*}
	rm -f .faux.pb
	touch $(PROTOBUFS)

.PHONY: test
test: gen $(HANDWRITTEN) ## Run unit tests
	@$(GO) test -vet=off -race -cover ./...
	@cd internal/crosstest && $(GO) test -vet=off -race ./...

.PHONY: bench
BENCH ?= .
bench: gen $(HANDWRITTEN) ## Run benchmarks
	@$(GO) version
	@cd internal/crosstest && $(GO) test -vet=off -bench=$(BENCH) $(BENCHFLAGS) -run="^$$" .

.PHONY: lint
lint: lintpb bin/gofmt bin/goimports bin/staticcheck ## Lint Go and protobuf
	@echo "Checking with gofmt..."
	@test -z "$$(./bin/gofmt -s -l . | tee /dev/stderr)"
	@echo "Checking with goimports..."
	@test -z "$$(./bin/goimports -local github.com/rerpc/rerpc -l $(HANDWRITTEN) | tee /dev/stderr)"
	@echo "Checking with go vet..."
	@$(GO) vet ./...
	@echo "Checking with staticcheck..."
	@bin/staticcheck -checks "inherit,ST1020,ST1021,ST1022" ./...

.PHONY: lintfix
lintfix: bin/gofmt bin/goimports ## Automatically fix some lint errors
	@./bin/gofmt -s -w .
	@./bin/goimports -local github.com/rerpc/rerpc -w $(HANDWRITTEN)

.PHONY: lintpb
lintpb: bin/buf $(PROTOBUFS)
	@echo "Checking with buf lint..."
	@./bin/buf lint
	@echo "Checking with buf breaking..."
	@./bin/buf breaking --against image_v1.bin

.PHONY: cover
cover: cover.out ## Browse coverage for the main package
	@$(GO) tool cover -html cover.out

cover.out: gen $(HANDWRITTEN)
	@$(GO) test -cover -coverprofile=$(@) .

.PHONY: gen
gen: .faux.pb ## Regenerate code

.faux.pb: $(PROTOBUFS) bin/buf bin/protoc-gen-go bin/protoc-gen-go-grpc bin/protoc-gen-twirp bin/protoc-gen-go-rerpc buf.gen.yaml
	./bin/buf generate
	rm internal/ping/v1test/ping{.twirp,_grpc.pb}.go
	rm internal/health/v1/health{.twirp,_grpc.pb}.go
	rm internal/reflection/v1alpha1/reflection{.twirp,_grpc.pb}.go
	touch $(@)

# Don't make this depend on $(PROTOBUFS), since we don't want to keep
# regenerating it.
image_v1.bin: bin/buf
	./bin/buf build -o $(@)

bin/protoc-gen-go: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go

bin/protoc-gen-go-grpc: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc

bin/protoc-gen-go-rerpc: $(shell ls cmd/protoc-gen-go-rerpc/*.go) go.mod
	$(GO) build -o $(@) ./cmd/protoc-gen-go-rerpc

bin/protoc-gen-twirp: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install github.com/twitchtv/twirp/protoc-gen-twirp

bin/buf: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install github.com/bufbuild/buf/cmd/buf

bin/gofmt:
	$(GO) build -o $(@) cmd/gofmt

bin/goimports: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install golang.org/x/tools/cmd/goimports

bin/staticcheck: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install honnef.co/go/tools/cmd/staticcheck

