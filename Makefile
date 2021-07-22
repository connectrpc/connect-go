# See https://tech.davis-hansson.com/p/make/
SHELL := bash
.DELETE_ON_ERROR:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rules

HANDWRITTEN=$(shell find . -name '*.go' | grep -v -e '\.pb\.go$$' -e '_string.go')
MODULE=github.com/akshayjshah/rerpc

.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "%-30s %s\n", $$1, $$2}'

.PHONY: clean
clean: ## Delete build output and generated code
   rm -f bin/protoc-gen-go-grpc
	rm -rf internal/statuspb/{v0,.faux}

.PHONY: test
test: gen $(HANDWRITTEN) ## Run unit tests
	go test -race ./...
	cd internal/crosstest && go test -race ./...

.PHONY: gen
gen: genpb ## Regenerate code

.PHONY: genpb
genpb: internal/statuspb/.faux

internal/statuspb/.faux: internal/statuspb/status.proto
	protoc internal/statuspb/status.proto \
		--go_out=. \
		--go_opt=module=$(MODULE)
	touch $(@)

bin/protoc-gen-go-grpc: internal/crosstest/go.mod
	GOBIN=$(PWD)/bin cd internal/crosstest && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc
	touch $(@)

