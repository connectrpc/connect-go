#!/bin/bash
# Regenerates the build-harness scaffold stubs in this directory from the repo's
# ping.proto, via buf.gen.yaml. The connect v1 and protobuf plugin versions come
# from go.mod.txt. The v2 connect plugin is built from this repo's local source
# (./cmd/protoc-gen-connect-go) so a refresh tracks HEAD. Set BUF to reuse an
# existing buf binary instead of installing the pinned one.
set -euo pipefail
cd "$(dirname "$0")"
build_dir="$(pwd)"
repo_root="$(cd ../../../.. && pwd)"
GO="${GO:-go}"
BUF_VERSION="1.69.0"

connect_version="$(awk '$1 == "connectrpc.com/connect" { print $2 }' go.mod.txt)"
protobuf_version="$(awk '$1 == "google.golang.org/protobuf" { print $2 }' go.mod.txt)"

bindir="$(mktemp -d)"
trap 'rm -rf "$bindir"' EXIT

# The v1 and v2 connect plugins share the binary name, so build them under
# distinct names that buf.gen.yaml references.
GOBIN="$bindir" $GO install "google.golang.org/protobuf/cmd/protoc-gen-go@${protobuf_version}"
GOBIN="$bindir" $GO install "connectrpc.com/connect/cmd/protoc-gen-connect-go@${connect_version}"
mv "$bindir/protoc-gen-connect-go" "$bindir/protoc-gen-connect-go-v1"
(cd "$repo_root" && $GO build -o "$bindir/protoc-gen-connect-go-v2" ./cmd/protoc-gen-connect-go)

buf_bin="${BUF:-}"
if [ -z "$buf_bin" ]; then
	GOBIN="$bindir" $GO install "github.com/bufbuild/buf/cmd/buf@v${BUF_VERSION}"
	buf_bin="$bindir/buf"
fi

cd "$repo_root"
PATH="$bindir:$PATH" "$buf_bin" generate \
	--template "${build_dir}/buf.gen.yaml" \
	--path internal/proto/connect/ping/v1/ping.proto \
	-o "${build_dir}"

echo "Regenerated scaffold stubs in ${build_dir}"
