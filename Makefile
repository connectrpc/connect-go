# See https://tech.davis-hansson.com/p/make/
SHELL := bash
.DELETE_ON_ERROR:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rules

HANDWRITTEN=$(shell find . -type f -name '*.go' | grep -v -e '\.pb\.go$$' -e '\.twirp\.go$$' -e '_string.go$$')
PROTOBUFS=$(shell find . -type f -name '*.proto')

.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "%-30s %s\n", $$1, $$2}'

.PHONY: clean
clean: ## Delete build output
	rm -f bin/{buf,goimports,staticcheck,protoc-gen-*}
	rm -f .faux.pb
	touch $(PROTOBUFS)

.PHONY: test
test: gen $(HANDWRITTEN) ## Run unit tests
	@go test -vet=off -race -cover ./...
	@cd internal/crosstest && go test -vet=off -race ./...

.PHONY: lint
lint: lintpb bin/goimports bin/staticcheck ## Lint Go and protobuf
	@echo "Checking with gofmt..."
	@test -z "$$(gofmt -s -l . | tee /dev/stderr)"
	@echo "Checking with goimports..."
	@test -z "$$(./bin/goimports -local github.com/rerpc/rerpc -l $(HANDWRITTEN) | tee /dev/stderr)"
	@echo "Checking with go vet..."
	@go vet ./...
	@echo "Checking with staticcheck..."
	@staticcheck -checks "inherit,ST1020,ST1021,ST1022" ./...

.PHONY: lintfix
lintfix: ## Automatically fix some lint errors
	@gofmt -s -w .
	@./bin/goimports -local github.com/rerpc/rerpc -w $(HANDWRITTEN)

.PHONY: lintpb
lintpb: bin/buf $(PROTOBUFS)
	@echo "Checking with buf lint..."
	@./bin/buf lint
	@echo "Checking with buf breaking..."
	@./bin/buf breaking --against image_v1.bin

.PHONY: cover
cover: cover.out ## Browse coverage for the main package
	@go tool cover -html cover.out

cover.out: gen $(HANDWRITTEN)
	@go test -cover -coverprofile=$(@) .

.PHONY: gen
gen: .faux.pb ## Regenerate code

.faux.pb: $(PROTOBUFS) bin/buf bin/protoc-gen-go-grpc bin/protoc-gen-twirp bin/protoc-gen-go-rerpc buf.gen.yaml
	./bin/buf generate
	rm internal/ping/v1test/ping{.twirp,_grpc.pb}.go
	touch $(@)

# Don't make this depend on $(PROTOBUFS), since we don't want to keep
# regenerating it.
image_v1.bin: bin/buf
	./bin/buf build -o $(@)

bin/protoc-gen-go-grpc: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc

bin/protoc-gen-go-rerpc: $(shell ls cmd/protoc-gen-go-rerpc/*.go) go.mod
	go build -o $(@) ./cmd/protoc-gen-go-rerpc

bin/protoc-gen-twirp: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin go install github.com/twitchtv/twirp/protoc-gen-twirp

bin/buf: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin go install github.com/bufbuild/buf/cmd/buf

bin/goimports: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin go install golang.org/x/tools/cmd/goimports

bin/staticcheck: internal/crosstest/go.mod
	cd internal/crosstest && GOBIN=$(PWD)/bin go install honnef.co/go/tools/cmd/staticcheck

