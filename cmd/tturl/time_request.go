package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
	"github.com/tantosec/tturl/tth2"
)

type timeRequest struct {
	ID             int
	Request        *http.Request
	Body           []byte
	Protocol       curlblocks.Protocol
	Label          string
	Warmup         bool
	DeclaredLength *int64
	Defaults       []string
}

type timeRequests struct {
	Measured []timeRequest
	Priming  []timeRequest
}

func materialiseTimeRequests(ctx context.Context, plan *curlblocks.Plan,
	protocols *curlblocks.ProtocolFlags, roles requestRoleFlags,
) (timeRequests, error) {
	expanded, err := plan.Expand()
	if err != nil {
		return timeRequests{}, err
	}
	boundary, err := newMultipartBoundary(rand.Reader)
	if err != nil {
		return timeRequests{}, err
	}
	var result timeRequests
	for _, group := range expanded {
		request, err := group.Block.NewRequestWithMultipartBoundary(ctx, group.URL, boundary)
		if err != nil {
			return result, plan.BlockError(group.BlockIndex, err)
		}
		var body []byte
		if request.Body != nil {
			body, err = io.ReadAll(request.Body)
			closeErr := request.Body.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return result, plan.BlockError(group.BlockIndex, err)
			}
		}
		protocol := protocols.Get(group.Block)
		length, err := validateTimeRequest(request, body, protocol)
		if err != nil {
			return result, plan.BlockError(group.BlockIndex, err)
		}
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		request.Body, _ = request.GetBody()
		if len(body) == 0 {
			request.Body = http.NoBody
		}
		request.ContentLength = int64(len(body))
		if length != nil {
			request.ContentLength = *length
		}
		warmup := roles.warmupOnly.Get(group.Block)
		for _, label := range group.Labels {
			item := timeRequest{
				Request: request.Clone(ctx), Body: bytes.Clone(body), Protocol: protocol,
				Label: label.Display, Warmup: warmup, DeclaredLength: length,
				Defaults: group.Block.DefaultHeaderNames(),
			}
			if warmup {
				result.Priming = append(result.Priming, item)
			} else {
				result.Measured = append(result.Measured, item)
			}
		}
	}
	for i := range result.Measured {
		result.Measured[i].ID = i
	}
	for i := range result.Priming {
		result.Priming[i].ID = len(result.Measured) + i
	}
	if len(result.Measured) == 0 {
		return result, fmt.Errorf("time needs at least one measured request")
	}
	return result, nil
}

func validateTimeRequest(request *http.Request, body []byte, protocol curlblocks.Protocol) (*int64, error) {
	selected := timing.HTTP2
	if protocol == curlblocks.HTTP11 {
		selected = timing.HTTP11
	}
	info, err := timing.ValidateRequest(timing.Request{HTTP: request, Body: body, Protocol: selected}, timing.Plan{})
	if err != nil {
		return nil, err
	}
	if !info.DeclaredContentLength.Present {
		return nil, nil
	}
	length := info.DeclaredContentLength.Value
	return &length, nil
}

func timePreviewRequest(item timeRequest) *http.Request {
	request := item.Request.Clone(item.Request.Context())
	if item.Protocol != curlblocks.HTTP2 {
		if item.DeclaredLength != nil {
			request.Header.Set("Content-Length", strconv.FormatInt(*item.DeclaredLength, 10))
		}
		return request
	}
	for _, value := range request.Header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			name := http.CanonicalHeaderKey(strings.Trim(token, " \t"))
			if name != "Content-Length" && !strings.EqualFold(name, "TE") {
				request.Header.Del(name)
			}
		}
	}
	stripHopByHopHeaders(request.Header)
	request.Header.Del("HTTP2-Settings")
	if item.DeclaredLength != nil {
		request.Header.Set("Content-Length", strconv.FormatInt(*item.DeclaredLength, 10))
	}
	return request
}

func formatTimeRequest(item timeRequest, fullBody bool) string {
	request := timePreviewRequest(item)
	var b strings.Builder
	if item.Protocol == curlblocks.HTTP2 {
		for _, field := range tth2.PseudoHeaders(request) {
			fmt.Fprintf(&b, "%s: %s\n", field.Name, curlblocks.DisplayText(field.Value))
		}
	} else {
		fmt.Fprintf(&b, "%s %s HTTP/1.1\n", curlblocks.DisplayText(request.Method),
			curlblocks.DisplayText(request.URL.RequestURI()))
		authority := request.Host
		if authority == "" {
			authority = request.URL.Host
		}
		fmt.Fprintf(&b, "Host: %s\n", curlblocks.DisplayText(authority))
	}
	names := make([]string, 0, len(request.Header))
	for name := range request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, "Host") {
			continue
		}
		displayName := name
		if item.Protocol == curlblocks.HTTP2 {
			displayName = strings.ToLower(name)
		}
		for _, value := range request.Header[name] {
			if fullBody {
				fmt.Fprintf(&b, "%s: %s\n", curlblocks.DisplayText(displayName), curlblocks.DisplayText(value))
			} else {
				fmt.Fprintln(&b, renderHeader(displayName, value))
			}
		}
	}
	if body, ok := bodyForDisplay(request, fullBody); ok {
		fmt.Fprintf(&b, "\n%s\n", curlblocks.DisplayText(body))
	}

	return b.String()
}
