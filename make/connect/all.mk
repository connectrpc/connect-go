GO_BINS := $(GO_BINS) \
	cmd/protoc-gen-go-connect

# TODO: this doesn't work because of the multiple go modules
# A lot of other stuff might not work because of this also
#GO_ALL_REPO_PKGS := ./cmd/... ./internal/... ./internal/crosstest/...

GIT_FILE_IGNORES := $(GIT_FILE_IGNORES) \
	cover.out \
	*.pprof \
	*.svg \
	/internal/crosstest/crosstest.test

# TODO: remove when golangci-lint works with 1.18
SKIP_GOLANGCI_LINT := 1

#LICENSE_HEADER_LICENSE_TYPE := apache
#LICENSE_HEADER_COPYRIGHT_HOLDER := Buf Technologies, Inc.
#LICENSE_HEADER_YEAR_RANGE := 2021-2022
#LICENSE_HEADER_IGNORES := \/testdata

BUF_LINT_INPUT := .

include make/go/bootstrap.mk
include make/go/go.mk
include make/go/buf.mk
#include make/go/license_header.mk
include make/go/dep_protoc_gen_go.mk
include make/go/dep_protoc_gen_go_grpc.mk

BENCH ?= .

bufgeneratedeps:: $(BUF) $(PROTOC_GEN_GO) $(PROTOC_GEN_GRPC_GO) installprotoc-gen-go-connect

.PHONY: bufgeneratecleango
bufgeneratecleango:
	rm -rf internal/gen/proto

.PHONY: bufgeneratecleancrosstestgo
bufgeneratecleancrosstestgo:
	rm -rf internal/crosstest/gen/proto

bufgenerateclean:: bufgeneratecleango bufgeneratecleancrosstestgo

.PHONY: bufgeneratego
bufgeneratego:
	buf generate

.PHONY: bufgeneratecrosstestgo
bufgeneratecrosstestgo:
	cd internal/crosstest && buf generate

bufgeneratesteps:: bufgeneratego bufgeneratecrosstestgo

.PHONY: bench
bench: gen $(HANDWRITTEN) ## Run benchmarks
	@go version
	@cd internal/crosstest && go test -vet=off -bench=$(BENCH) $(BENCHFLAGS) -run="^$$" .
