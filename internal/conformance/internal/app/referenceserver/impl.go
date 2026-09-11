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

package referenceserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/internal/conformance/internal"
	conformancev1 "connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1"
	"connectrpc.com/connect/v2/internal/conformance/internal/gen/connectrpc/conformance/v1/conformancev1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const serverName = "connectconformance-referenceserver"

// ConformanceRequest is a general interface for all conformance requests (UnaryRequest, ServerStreamRequest, etc.)
type ConformanceRequest interface {
	GetResponseHeaders() []*conformancev1.Header
	GetResponseTrailers() []*conformancev1.Header
}

type conformanceServer struct {
	conformancev1connect.UnimplementedConformanceServiceHandler
}

func (s *conformanceServer) Unary(
	ctx context.Context,
	req *conformancev1.UnaryRequest,
) (*conformancev1.UnaryResponse, error) {
	return doUnary(ctx, req, func(payload *conformancev1.ConformancePayload) *conformancev1.UnaryResponse {
		return &conformancev1.UnaryResponse{
			Payload: payload,
		}
	})
}

func (s *conformanceServer) IdempotentUnary(
	ctx context.Context,
	req *conformancev1.IdempotentUnaryRequest,
) (*conformancev1.IdempotentUnaryResponse, error) {
	return doUnary(ctx, req, func(payload *conformancev1.ConformancePayload) *conformancev1.IdempotentUnaryResponse {
		return &conformancev1.IdempotentUnaryResponse{
			Payload: payload,
		}
	})
}

type hasUnaryResponseDefinition[T any] interface {
	*T
	proto.Message
	GetResponseDefinition() *conformancev1.UnaryResponseDefinition
}

func doUnary[ReqT, RespT any, Req hasUnaryResponseDefinition[ReqT]](
	ctx context.Context,
	req *ReqT,
	makeResp func(payload *conformancev1.ConformancePayload) *RespT,
) (*RespT, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	msg := Req(req)
	msgAsAny, err := asAny(msg)
	if err != nil {
		return nil, err
	}
	payload, connectErr := parseUnaryResponseDefinition(
		ctx,
		msg.GetResponseDefinition(),
		info,
		queryParamsFromContext(ctx),
		[]*anypb.Any{msgAsAny},
	)
	if connectErr != nil {
		return nil, connectErr
	}

	if msg.GetResponseDefinition() != nil {
		internal.AddHeaders(msg.GetResponseDefinition().ResponseHeaders, info.ResponseHeader())
		internal.AddHeaders(msg.GetResponseDefinition().ResponseTrailers, info.ResponseTrailer())

		// If a response delay was specified, sleep for that amount of ms before responding
		responseDelay := time.Duration(msg.GetResponseDefinition().ResponseDelayMs) * time.Millisecond
		time.Sleep(responseDelay)
	}

	return makeResp(payload), nil
}

func (s *conformanceServer) ClientStream(
	ctx context.Context,
	stream conformancev1connect.ConformanceServiceClientStreamServerStream,
) (*conformancev1.ClientStreamResponse, error) {
	info, _ := connect.CallInfoForServerContext(ctx)
	var responseDefinition *conformancev1.UnaryResponseDefinition
	firstRecv := true
	var reqs []*anypb.Any
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// If this is the first message received on the stream, save off the response definition we need to send
		if firstRecv {
			responseDefinition = msg.ResponseDefinition
			firstRecv = false
		}
		// Record all the requests received
		msgAsAny, err := asAny(msg)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, msgAsAny)
	}

	payload, err := parseUnaryResponseDefinition(
		ctx,
		responseDefinition,
		info,
		queryParamsFromContext(ctx),
		reqs,
	)
	if err != nil {
		return nil, err
	}

	if responseDefinition != nil {
		internal.AddHeaders(responseDefinition.ResponseHeaders, info.ResponseHeader())
		internal.AddHeaders(responseDefinition.ResponseTrailers, info.ResponseTrailer())

		// If a response delay was specified, sleep for that amount of ms before responding
		responseDelay := time.Duration(responseDefinition.ResponseDelayMs) * time.Millisecond
		time.Sleep(responseDelay)
	}

	return &conformancev1.ClientStreamResponse{Payload: payload}, nil
}

func (s *conformanceServer) ServerStream(
	ctx context.Context,
	req *conformancev1.ServerStreamRequest,
	stream conformancev1connect.ConformanceServiceServerStreamServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	// Convert the request to an Any so that it can be recorded in the payload
	msgAsAny, err := asAny(req)
	if err != nil {
		return err
	}

	respNum := 0

	responseDefinition := req.ResponseDefinition
	if responseDefinition != nil { //nolint:nestif
		internal.AddHeaders(responseDefinition.ResponseHeaders, info.ResponseHeader())
		internal.AddHeaders(responseDefinition.ResponseTrailers, info.ResponseTrailer())

		if len(responseDefinition.ResponseData) > 0 {
			// Immediately send the headers/trailers on the stream so that they can be read by the client
			if err := stream.SendHeaders(); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
			}
		}

		// Calculate the response delay if specified
		responseDelay := time.Duration(responseDefinition.ResponseDelayMs) * time.Millisecond

		for _, data := range responseDefinition.ResponseData {
			resp := &conformancev1.ServerStreamResponse{
				Payload: &conformancev1.ConformancePayload{
					Data: data,
				},
			}

			// Only set the request info if this is the first response being sent back
			// because for server streams, nothing in the request info will change
			// after the first response.
			if respNum == 0 {
				resp.Payload.RequestInfo = createRequestInfo(ctx, info.RequestHeader(), queryParamsFromContext(ctx), []*anypb.Any{msgAsAny})
			}

			// If a response delay was specified, sleep for that amount of ms before responding
			time.Sleep(responseDelay)

			if err := stream.Send(resp); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
			}
			respNum++
		}

		if responseDefinition.Error != nil {
			if respNum == 0 {
				// We've sent no responses and are returning an error, so build a
				// RequestInfo message and append to the error details
				reqInfo := createRequestInfo(ctx, info.RequestHeader(), queryParamsFromContext(ctx), []*anypb.Any{msgAsAny})
				reqInfoAny, err := anypb.New(reqInfo)
				if err != nil {
					return connect.NewError(connect.CodeInternal, err.Error())
				}
				responseDefinition.Error.Details = append(responseDefinition.Error.Details, reqInfoAny)
			}
			return internal.ConvertProtoToConnectError(responseDefinition.Error)
		}
	}

	return nil
}

func (s *conformanceServer) BidiStream(
	ctx context.Context,
	stream conformancev1connect.ConformanceServiceBidiStreamServerStream,
) error {
	info, _ := connect.CallInfoForServerContext(ctx)
	var responseDefinition *conformancev1.StreamResponseDefinition
	var responseDelay time.Duration
	fullDuplex := false
	firstRecv := true
	respNum := 0
	var reqs []*anypb.Any
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Reads are done, break the receive loop and send any remaining responses
				break
			}
			return fmt.Errorf("receive request: %w", err)
		}

		// Record all requests received
		msgAsAny, err := asAny(req)
		if err != nil {
			return err
		}
		reqs = append(reqs, msgAsAny)

		// If this is the first message in the stream, save off the total responses we need to send
		// plus whether this should be full or half duplex
		if firstRecv { //nolint:nestif
			responseDefinition = req.ResponseDefinition
			fullDuplex = req.FullDuplex
			firstRecv = false

			// If a response definition was provided, add the headers and trailers
			if responseDefinition != nil {
				internal.AddHeaders(responseDefinition.ResponseHeaders, info.ResponseHeader())
				internal.AddHeaders(responseDefinition.ResponseTrailers, info.ResponseTrailer())

				if fullDuplex && len(responseDefinition.ResponseData) > 0 {
					// Immediately send the headers on the stream so that they can be read by the client.
					// We can only do this for full-duplex. For half-duplex operation, we must let client
					// complete its upload before trying to send anything.
					if err := stream.SendHeaders(); err != nil {
						return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
					}
				}

				// Calculate a response delay if specified
				responseDelay = time.Duration(responseDefinition.ResponseDelayMs) * time.Millisecond
			}
		}

		// If fullDuplex, then send one of the desired responses each time we get a message on the stream
		if fullDuplex {
			if respNum >= len(responseDefinition.GetResponseData()) {
				// If there are no responses to send, then break the receive loop
				// and throw the error specified
				break
			}

			resp := &conformancev1.BidiStreamResponse{
				Payload: &conformancev1.ConformancePayload{
					Data: responseDefinition.ResponseData[respNum],
				},
			}
			var requestInfo *conformancev1.ConformancePayload_RequestInfo
			if respNum == 0 {
				// Only send the full request info (including headers and timeouts)
				// in the first response
				requestInfo = createRequestInfo(ctx, info.RequestHeader(), queryParamsFromContext(ctx), reqs)
			} else {
				// All responses after the first should only include the requests
				// since that is the only thing that will change between responses
				// for a full duplex stream
				requestInfo = &conformancev1.ConformancePayload_RequestInfo{
					Requests: reqs,
				}
			}
			resp.Payload.RequestInfo = requestInfo

			// If a response delay was specified, sleep for that amount of ms before responding
			time.Sleep(responseDelay)

			if err := stream.Send(resp); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
			}
			respNum++
			reqs = nil
		}
	}

	if !fullDuplex && len(responseDefinition.GetResponseData()) > 0 {
		// Now that upload is complete, we can immediately send headers for half-duplex calls.
		if err := stream.SendHeaders(); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
		}
	}

	// If we still have responses left to send, flush them now. This accommodates
	// both scenarios of half duplex (we haven't sent any responses yet) or full duplex
	// where the requested responses are greater than the total requests.
	if responseDefinition != nil { //nolint:nestif
		for ; respNum < len(responseDefinition.ResponseData); respNum++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			resp := &conformancev1.BidiStreamResponse{
				Payload: &conformancev1.ConformancePayload{
					Data: responseDefinition.ResponseData[respNum],
				},
			}
			// Only set the request info if this is the first response being sent back
			// because for half duplex streams, nothing in the request info will change
			// after the first response (this includes the requests since they've all
			// been received by this point)
			if respNum == 0 {
				resp.Payload.RequestInfo = createRequestInfo(ctx, info.RequestHeader(), queryParamsFromContext(ctx), reqs)
			}

			// If a response delay was specified, sleep for that amount of ms before responding
			time.Sleep(responseDelay)

			if err := stream.Send(resp); err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("error sending on stream: %w", err).Error())
			}
		}

		if responseDefinition.Error != nil {
			if respNum == 0 {
				// We've sent no responses and are returning an error, so build a
				// RequestInfo message and append to the error details
				reqInfo := createRequestInfo(ctx, info.RequestHeader(), queryParamsFromContext(ctx), reqs)
				reqInfoAny, err := anypb.New(reqInfo)
				if err != nil {
					return connect.NewError(connect.CodeInternal, err.Error())
				}
				responseDefinition.Error.Details = append(responseDefinition.Error.Details, reqInfoAny)
			}
			return internal.ConvertProtoToConnectError(responseDefinition.Error)
		}
	}

	return nil
}

// Parses the given unary response definition and returns either
// a built payload or a connect error based on the definition.
func parseUnaryResponseDefinition(
	ctx context.Context,
	def *conformancev1.UnaryResponseDefinition,
	info *connect.CallInfo,
	queryParams url.Values,
	reqs []*anypb.Any,
) (*conformancev1.ConformancePayload, *connect.Error) {
	reqInfo := createRequestInfo(ctx, info.RequestHeader(), queryParams, reqs)
	if def == nil {
		// If the definition is not set at all, there's nothing to respond with.
		// Just return a payload with the request info
		return &conformancev1.ConformancePayload{
			RequestInfo: reqInfo,
		}, nil
	}

	switch respType := def.Response.(type) {
	case *conformancev1.UnaryResponseDefinition_Error:
		// The server should add the request info to the error details
		// for unary responses that return an error.
		reqInfoAny, err := anypb.New(reqInfo)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error())
		}
		respType.Error.Details = append(respType.Error.Details, reqInfoAny)

		connectErr := internal.ConvertProtoToConnectError(respType.Error)

		// Set the response headers and trailers on the call info.
		internal.AddHeaders(def.GetResponseHeaders(), info.ResponseHeader())
		internal.AddHeaders(def.GetResponseTrailers(), info.ResponseTrailer())

		return nil, connectErr

	case *conformancev1.UnaryResponseDefinition_ResponseData, nil:
		payload := &conformancev1.ConformancePayload{
			RequestInfo: reqInfo,
		}

		// If response data was provided, set that in the payload response
		if respType, ok := respType.(*conformancev1.UnaryResponseDefinition_ResponseData); ok {
			payload.Data = respType.ResponseData
		}
		return payload, nil
	default:
		return nil, connect.Errorf(connect.CodeInvalidArgument, "provided UnaryRequest.Response has an unexpected type %T", respType)
	}
}

// Creates request info for a conformance payload.
func createRequestInfo(
	ctx context.Context,
	headers *connect.Header,
	queryParams url.Values,
	reqs []*anypb.Any,
) *conformancev1.ConformancePayload_RequestInfo {
	headerInfo := internal.ConvertToProtoHeader(headers)

	var connectGetInfo *conformancev1.ConformancePayload_ConnectGetInfo
	if len(queryParams) > 0 {
		queryParamInfo := make([]*conformancev1.Header, 0, len(queryParams))
		for name, values := range queryParams {
			queryParamInfo = append(queryParamInfo, &conformancev1.Header{
				Name:  name,
				Value: values,
			})
		}
		connectGetInfo = &conformancev1.ConformancePayload_ConnectGetInfo{
			QueryParams: queryParamInfo,
		}
	}

	var timeoutMs *int64
	if timeout, ok := timeoutFromContext(ctx); ok {
		timeoutMs = proto.Int64(timeout.Milliseconds())
	}

	// Set all observed request headers and requests in the response payload
	return &conformancev1.ConformancePayload_RequestInfo{
		RequestHeaders: headerInfo,
		Requests:       reqs,
		TimeoutMs:      timeoutMs,
		ConnectGetInfo: connectGetInfo,
	}
}

// queryParamsFromContext returns the Connect GET query parameters for the
// in-flight request, or nil when it was not sent as an HTTP GET.
func queryParamsFromContext(ctx context.Context) url.Values {
	info, _ := connecthttp.ServerInfoForContext(ctx)
	return info.HTTPGetQueryParams()
}

func timeoutFromContext(ctx context.Context) (time.Duration, bool) {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline), true
	}
	return 0, false
}

// Converts the given message to an Any.
func asAny(msg proto.Message) (*anypb.Any, error) {
	msgAsAny, err := anypb.New(msg)
	if err != nil {
		return nil, connect.NewError(
			connect.CodeInternal,
			fmt.Errorf("unable to convert message: %w", err).Error(),
		)
	}
	return msgAsAny, nil
}

// serverNameHandlerInterceptor adds a "server" header on outgoing responses.
func serverNameHandlerInterceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		if info, ok := connect.CallInfoForServerContext(ctx); ok {
			// decorate server with the program name and version
			existing := info.ResponseHeader().Get("Server")
			info.ResponseHeader().Set("Server", strings.TrimSpace(fmt.Sprintf("%s %s/%s", existing, serverName, internal.Version)))
		}
		return next(ctx, spec, stream)
	}
}
