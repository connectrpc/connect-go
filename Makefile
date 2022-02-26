# See https://tech.davis-hansson.com/p/make/
SHELL := bash
.DELETE_ON_ERROR:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rules
BENCHFLAGS ?= -cpuprofile=cpu.pprof -memprofile=mem.pprof -benchmem -benchtime=30s
GO ?= go

HANDWRITTEN=$(shell find . -type f -name '*.go' | grep -v -e '\.pb\.go$$' -e '_string.go$$')
PROTOBUFS=$(shell find . -type f -name '*.proto')

.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "%-30s %s\n", $$1, $$2}'

.PHONY: clean
clean: ## Delete build output
	rm -f bin/{buf,gofmt,goimports,staticcheck,protoc-gen-*}
	rm -f .faux.pb
	rm -rf internal/gen/proto internal/crosstest/gen/proto
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
	@test -z "$$(./bin/goimports -l $(HANDWRITTEN) | tee /dev/stderr)"
	@echo "Checking with go vet..."
	@$(GO) vet ./...
	@# TODO: re-enable when staticcheck supports type parameters
	@#echo "Checking with staticcheck..."
	@# See https://github.com/dominikh/go-tools/issues/1166
	@#bin/staticcheck -checks "inherit,ST1020,ST1021,ST1022" ./...

.PHONY: lintfix
lintfix: bin/gofmt bin/goimports ## Automatically fix some lint errors
	@./bin/gofmt -s -w .
	@./bin/goimports -w $(HANDWRITTEN)

.PHONY: lintpb
lintpb: bin/buf internal/proto/buf.yaml internal/crosstest/proto/buf.yaml $(PROTOBUFS)
	@echo "Checking with buf lint..."
	@./bin/buf lint
	@cd internal/crosstest && ../../bin/buf lint

.PHONY: cover
cover: cover.out ## Browse coverage for the main package
	@$(GO) tool cover -html cover.out

cover.out: gen $(HANDWRITTEN)
	@$(GO) test -cover -coverprofile=$(@) .

.PHONY: gen
gen: .faux.pb ## Regenerate code

.faux.pb: $(PROTOBUFS) bin/buf bin/protoc-gen-go bin/protoc-gen-go-grpc bin/protoc-gen-go-connect buf.work.yaml buf.gen.yaml internal/proto/buf.yaml internal/crosstest/buf.work.yaml internal/crosstest/buf.gen.yaml internal/crosstest/proto/buf.yaml
	./bin/buf generate
	cd internal/crosstest && ../../bin/buf generate
	touch $(@)

bin/protoc-gen-go: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go

bin/protoc-gen-go-grpc: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc

bin/protoc-gen-go-connect: $(shell ls cmd/protoc-gen-go-connect/*.go) go.mod
	$(GO) build -o $(@) ./cmd/protoc-gen-go-connect

bin/buf: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install github.com/bufbuild/buf/cmd/buf

bin/gofmt:
	$(GO) build -o $(@) cmd/gofmt

bin/goimports: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install golang.org/x/tools/cmd/goimports

bin/staticcheck: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin $(GO) install honnef.co/go/tools/cmd/staticcheck

