#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

BINDIR="../../.tmp/bin"
mkdir -p $BINDIR
GO="${GO:-go}"

# connectconformance is the upstream test runner. The reference client and
# server are built from this repo's local copies (./cmd/...) because they've
# been ported to connect-go v2 and aren't upstreamed yet; via the go.mod
# replace directive they exercise this repo's HEAD, so running them effectively
# tests changes in this repo. Once the v2 reference implementations are
# upstreamed, these will return to being built from
# connectrpc.com/conformance/cmd/reference{client,server}.
$GO build -o $BINDIR/connectconformance connectrpc.com/conformance/cmd/connectconformance
$GO build -o $BINDIR/referenceclient ./cmd/referenceclient
$GO build -o $BINDIR/referenceserver ./cmd/referenceserver

echo "Running conformance tests against client..."
$BINDIR/connectconformance --mode client --conf config.yaml -v --trace -- $BINDIR/referenceclient

echo "Running conformance tests against server..."
$BINDIR/connectconformance --mode server --conf config.yaml -v --trace -- $BINDIR/referenceserver
