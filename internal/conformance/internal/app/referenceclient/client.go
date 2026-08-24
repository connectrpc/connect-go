// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package referenceclient

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"connectrpc.com/connect/v2/connectgzip"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/conformance/internal"
	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
	"connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1/conformancev1connect"
	"golang.org/x/sync/semaphore"
)

// Run runs the client according to a client config read from the 'in' reader. The result of the run
// is written to the 'out' writer, including any errors encountered during the actual run. Any error
// returned from this function is indicative of an issue with the reader or writer and should not be related
// to the actual run.
func Run(ctx context.Context, args []string, inReader io.ReadCloser, outWriter, errWriter io.WriteCloser) error {
	return run(ctx, args, inReader, outWriter, errWriter)
}

func run(ctx context.Context, args []string, inReader io.ReadCloser, outWriter, _ io.WriteCloser) (retErr error) {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	json := flags.Bool("json", false, "whether to use the JSON format for marshaling / unmarshaling messages")
	parallel := flags.Uint("p", uint(runtime.GOMAXPROCS(0))*4, "the number of parallel RPCs to issue")
	showVersion := flags.Bool("version", false, "show version and exit")

	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *showVersion {
		_, _ = fmt.Fprintf(outWriter, "%s %s\n", filepath.Base(args[0]), internal.Version)
		return nil
	}
	if flags.NArg() != 0 {
		return errors.New("this command does not accept any positional arguments")
	}
	if *parallel == 0 {
		return errors.New("invalid parallelism; must be greater than zero")
	}

	codec := internal.NewCodec(*json)
	decoder := codec.NewDecoder(inReader)
	encoder := codec.NewEncoder(outWriter)
	var encoderMu sync.Mutex

	var failure atomic.Pointer[error]
	defer func() {
		// if we're about to return nil error, but a goroutine reported
		// a failure, return that failure as the error
		if errPtr := failure.Load(); errPtr != nil && retErr == nil {
			retErr = *errPtr
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	sema := semaphore.NewWeighted(int64(*parallel))

	var transports transports

	for {
		var req conformancev1.ClientCompatRequest
		err := decoder.DecodeNext(&req)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if err := sema.Acquire(ctx, 1); err != nil {
			return err
		}
		if errPtr := failure.Load(); errPtr != nil {
			// If there's already been a terminal failure, don't spawn
			// anymore goroutines.
			return *errPtr
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sema.Release(1)

			result, err := invoke(ctx, &transports, &req)

			// Build the result for the out writer.
			resp := &conformancev1.ClientCompatResponse{
				TestName: req.TestName,
			}
			// If an error was returned, it was a runtime / unexpected internal error so
			// the written response should contain an error result, not a response with
			// any RPC information
			if err != nil {
				resp.Result = &conformancev1.ClientCompatResponse_Error{
					Error: &conformancev1.ClientErrorResult{
						Message: err.Error(),
					},
				}
			} else {
				// clear out reference-mode-specific details
				result.HttpStatusCode = nil
				result.Feedback = nil
				resp.Result = &conformancev1.ClientCompatResponse_Response{
					Response: result,
				}
			}

			// Marshal the response and write the output
			func() {
				encoderMu.Lock()
				defer encoderMu.Unlock()
				if err := encoder.Encode(resp); err != nil {
					failure.CompareAndSwap(nil, &err)
				}
			}()
		}()
	}
}

// Invokes a ClientCompatRequest, returning either the result of the invocation or an error. The error
// returned from this function indicates a runtime/unexpected internal error and is not indicative of a
// Connect error returned from calling an RPC. Any error (i.e. a Connect error) that _is_ returned from
// the actual RPC invocation will be present in the returned ClientResponseResult.
func invoke(ctx context.Context, transports *transports, req *conformancev1.ClientCompatRequest) (*conformancev1.ClientResponseResult, error) {
	transport, serverURL, err := transports.get(req)
	if err != nil {
		return nil, err
	}

	// Create client options based on protocol of the implementation
	clientOptions := []connecthttp.Option{connecthttp.WithHTTPGet()}
	switch req.Protocol {
	case conformancev1.Protocol_PROTOCOL_GRPC:
		clientOptions = append(clientOptions, connecthttp.WithGRPC())
	case conformancev1.Protocol_PROTOCOL_GRPC_WEB:
		clientOptions = append(clientOptions, connecthttp.WithGRPCWeb())
	case conformancev1.Protocol_PROTOCOL_CONNECT:
		// Do nothing
	case conformancev1.Protocol_PROTOCOL_UNSPECIFIED:
		return nil, errors.New("a protocol must be specified")
	}

	switch req.Codec {
	case conformancev1.Codec_CODEC_PROTO:
		// this is the default, no option needed
	case conformancev1.Codec_CODEC_JSON:
		jsonCodec := internal.NewStrictJSONCodec()
		clientOptions = append(clientOptions,
			connecthttp.WithCodec(jsonCodec),
			connecthttp.WithSendCodec(jsonCodec.Name()),
		)
	case conformancev1.Codec_CODEC_TEXT: //nolint:staticcheck // staticcheck complains because this const is deprecated
		return nil, fmt.Errorf("%s is deprecated and should not be used", req.Codec)
	default:
		return nil, errors.New("a codec must be specified")
	}

	switch req.Compression {
	case conformancev1.Compression_COMPRESSION_GZIP:
		// Connect clients send uncompressed requests and ask for gzipped responses by default
		// As a result, specifying a compression of gzip for a client indicates it should also
		// send gzipped requests
		gzipCompressor := connectgzip.New()
		clientOptions = append(clientOptions,
			connecthttp.WithCompressor(gzipCompressor),
			connecthttp.WithSendCompression(gzipCompressor.Name()),
		)
	case conformancev1.Compression_COMPRESSION_IDENTITY, conformancev1.Compression_COMPRESSION_UNSPECIFIED:
		// No compression; do nothing
	default:
		return nil, fmt.Errorf("compression %v is not supported (the v2 reference client supports gzip only)", req.Compression)
	}

	if req.MessageReceiveLimit > 0 {
		clientOptions = append(clientOptions, connecthttp.WithReadMaxBytes(int(req.MessageReceiveLimit)))
	}

	switch req.GetService() {
	case conformancev1connect.ConformanceServiceName:
		return newInvoker(transport, serverURL, clientOptions).Invoke(ctx, req)
	default:
		return nil, fmt.Errorf("service name %s is not a valid service", req.GetService())
	}
}
