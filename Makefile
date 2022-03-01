# See https://tech.davis-hansson.com/p/make/
SHELL := bash
.DELETE_ON_ERROR:
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rules
# Set to use a different compiler. For example, `GO=go1.18rc1 make test`.
GO ?= go
# Which commit of bufbuild/makego should we source checknodiffgenerated.bash
# from?
MAKEGO_COMMIT := 383cdab9b837b1fba0883948ff54ed20eedbd611

HANDWRITTEN=$(shell find . -type f -name '*.go' | grep -v -e '\.pb\.go$$')
PROTOBUFS=$(shell find . -type f -name '*.proto')

# External Go binaries used in testing, linting, or code generation. Add new
# binaries by running `go get $YOUR_TOOL` in internal/tools and adding the
# import path here.
EXTERNAL_GO_TOOLS := \
	github.com/bufbuild/buf/cmd/buf \
	github.com/bufbuild/buf/private/pkg/licenseheader/cmd/license-header \
	golang.org/x/tools/cmd/goimports \
	google.golang.org/protobuf/cmd/protoc-gen-go

.PHONY: help
help: ## Describe useful make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "%-30s %s\n", $$1, $$2}'

.PHONY: clean
clean:: ## Delete build output and generated files
	rm -f faux.* bin/*
	rm -rf internal/gen/proto
	touch $(PROTOBUFS)

.PHONY: test
test: gen $(HANDWRITTEN) ## Run unit tests
	@$(GO) test -vet=off -race -cover ./...

.PHONY: lint
lint: lintpb bin/gofmt bin/goimports ## Lint Go and protobuf
	@echo "Checking with gofmt..."
	@test -z "$$(./bin/gofmt -s -l . | tee /dev/stderr)"
	@echo "Checking with goimports..."
	@test -z "$$(./bin/goimports -l $(HANDWRITTEN) | tee /dev/stderr)"
	@# TODO: replace vet with golangci-lint when it supports 1.18
	@# Configure staticcheck to target the correct Go version and enable
	@# ST1020, ST1021, and ST1022.
	@echo "Checking with go vet..."
	@$(GO) vet ./...

.PHONY: checkgen
checkgen: bin/checknodiffgenerated.bash ## Ensure generated code is up-to-date
	@bin/checknodiffgenerated.bash $(MAKE) --no-print-directory gen

.PHONY: lintfix
lintfix: bin/gofmt bin/goimports ## Automatically fix some lint errors
	@./bin/gofmt -s -w .
	@./bin/goimports -w $(HANDWRITTEN)

.PHONY: lintpb
lintpb: bin/buf internal/proto/buf.yaml $(PROTOBUFS)
	@echo "Checking with buf lint..."
	@./bin/buf lint

.PHONY: gen
gen: .faux.pb bin/license-header ## Regenerate code and licenses
	@bin/license-header \
		--license-type apache \
		--copyright-holder "Buf Technologies, Inc." \
		--year-range "2021-2022" \
		$(shell git ls-files) $(shell git ls-files --exclude-standard --others)

.faux.pb: $(PROTOBUFS) bin/buf bin/protoc-gen-go bin/protoc-gen-connect-go buf.gen.yaml buf.work.yaml internal/proto/buf.yaml
	./bin/buf generate
	touch $(@)

bin/gofmt:
	$(GO) build -o $(@) cmd/gofmt

bin/protoc-gen-connect-go:
	$(GO) build -o $(@) ./cmd/protoc-gen-connect-go

bin/checknodiffgenerated.bash:
	curl -o $(@) https://raw.githubusercontent.com/bufbuild/makego/$(MAKEGO_COMMIT)/make/go/scripts/checknodiffgenerated.bash
	chmod u+x $(@)

define install-go-bin
bin/$(notdir $1): internal/tools/go.mod
	cd internal/tools && GOBIN=$$(PWD)/bin $$(GO) install $1
endef
$(foreach gobin,$(EXTERNAL_GO_TOOLS),$(eval $(call install-go-bin,$(gobin))))
