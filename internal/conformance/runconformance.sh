#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

BINDIR="../../.tmp/bin"
mkdir -p $BINDIR
GO="${GO:-go}"

# These will get built using current HEAD of this connect-go repo
# thanks to replace directive in go.mod. So by testing the reference
# implementations (which are written with connect-go), we are effectively
# testing changes in this repo.
$GO build -o $BINDIR/connectconformance connectrpc.com/conformance/cmd/connectconformance
$GO build -o $BINDIR/referenceclient connectrpc.com/conformance/cmd/referenceclient
$GO build -o $BINDIR/referenceserver connectrpc.com/conformance/cmd/referenceserver

echo "Running conformance tests against client..."
$BINDIR/connectconformance --mode client --conf config.yaml --known-failing @known-failing.txt -v --trace -- $BINDIR/referenceclient

echo "Running conformance tests against server..."
$BINDIR/connectconformance --mode server --conf config.yaml -v --trace -- $BINDIR/referenceserver
