package timing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2/hpack"
)

// Resolve validates a plan without sources, output or target interaction.
// Its snapshots are independent of subsequent caller-owned request mutations.
func Resolve(plan Plan) (*Resolved, error) {
	if len(plan.Requests) == 0 {
		return nil, errors.New("at least one measured request is required")
	}
	if plan.Arrangement != ArrangeNone && plan.Arrangement != ArrangeRotate && plan.Arrangement != ArrangeRandom {
		return nil, errors.New("invalid arrangement")
	}
	if plan.Warmup < 0 || plan.Connections < 0 {
		return nil, errors.New("negative warmup or connection ceiling")
	}
	if plan.RequestTimeout < 0 || plan.RunTimeout < 0 || plan.ReleaseDelay < 0 {
		return nil, errors.New("negative timeout or release delay")
	}
	if plan.ReleaseDelay > 0 && !plan.LastByteSync {
		return nil, errors.New("release delay requires last-byte synchronisation")
	}
	if plan.RequestTimeout > 0 && plan.ReleaseDelay >= plan.RequestTimeout {
		return nil, errors.New("request timeout must exceed release delay")
	}
	for _, rate := range []float64{plan.BatchRate, plan.RequestRate} {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return nil, errors.New("rates must be finite and nonnegative")
		}
	}
	if plan.ReceiveHeaderMax <= 0 || plan.ReceiveHeaderMax > int64(math.MaxInt)-4096 {
		return nil, errors.New("receive header limit is not representable")
	}
	if plan.ResponseBodyMax < -1 || plan.Capture.Body && plan.Capture.BodyMax < -1 {
		return nil, errors.New("invalid response or capture body limit")
	}
	if plan.Priming != nil && (len(plan.Priming) == 0 || plan.Warmup == 0) {
		return nil, errors.New("a distinct priming set requires positive warmup")
	}
	resolved := &Resolved{Plan: plan, Width: len(plan.Requests), RequestedTrials: plan.Trials}
	resolved.Plan.Requests = nil
	resolved.Plan.Priming = nil
	if plan.TLSConfig != nil {
		resolved.Plan.TLSConfig = plan.TLSConfig.Clone()
	}
	seen := make(map[int]bool)
	poolIDs := make(map[string]int)
	add := func(input Request, priming bool) error {
		if input.ID < 0 || seen[input.ID] {
			return fmt.Errorf("request %d: identity must be unique and nonnegative", input.ID)
		}
		seen[input.ID] = true
		request, info, pool, err := resolveRequest(input, resolved.Plan)
		if err != nil {
			return fmt.Errorf("request %d: %w", input.ID, err)
		}
		key := pool.Scheme + "\x00" + pool.Host + "\x00" + pool.Port + "\x00" + string(pool.Protocol)
		poolID, present := poolIDs[key]
		if !present {
			if priming {
				return errors.New("priming set introduces an unused destination pool")
			}
			poolID = len(resolved.Pools)
			poolIDs[key] = poolID
			pool.ID = poolID
			resolved.Pools = append(resolved.Pools, pool)
		}
		info.PoolID = poolID
		if priming {
			resolved.Plan.Priming = append(resolved.Plan.Priming, request)
			resolved.Priming = append(resolved.Priming, info)
			resolved.Pools[poolID].PrimingRequestIDs = append(resolved.Pools[poolID].PrimingRequestIDs, input.ID)
		} else {
			resolved.Plan.Requests = append(resolved.Plan.Requests, request)
			resolved.Requests = append(resolved.Requests, info)
			resolved.Pools[poolID].RequestIDs = append(resolved.Pools[poolID].RequestIDs, input.ID)
			resolved.Pools[poolID].Width++
		}
		return nil
	}
	for _, request := range plan.Requests {
		if err := add(request, false); err != nil {
			return nil, err
		}
	}
	if plan.Priming == nil {
		resolved.Priming = slices.Clone(resolved.Requests)
		for index := range resolved.Pools {
			resolved.Pools[index].PrimingRequestIDs = slices.Clone(resolved.Pools[index].RequestIDs)
		}
	} else {
		for _, request := range plan.Priming {
			if err := add(request, true); err != nil {
				return nil, err
			}
		}
		for _, pool := range resolved.Pools {
			if plan.Synchronise && len(pool.PrimingRequestIDs) != pool.Width {
				return nil, errors.New("synchronised warmup set requires equal request counts in every measured pool")
			}
			if len(pool.PrimingRequestIDs) == 0 {
				return nil, errors.New("priming set does not cover every measured pool")
			}
		}
	}
	resolved.Destinations = len(resolved.Pools)
	resolved.ConnectionsPerWorker = resolved.Destinations
	if plan.Synchronise {
		resolved.ConnectionsPerWorker = resolved.Width
	}
	resolved.ConnectionCeiling = plan.Connections
	if resolved.ConnectionCeiling == 0 {
		resolved.ConnectionCeiling = resolved.ConnectionsPerWorker
	}
	if resolved.ConnectionCeiling < resolved.ConnectionsPerWorker {
		return nil, fmt.Errorf("connection ceiling requires at least %d", resolved.ConnectionsPerWorker)
	}
	resolved.Workers = resolved.ConnectionCeiling / resolved.ConnectionsPerWorker
	resolved.PlannedTrials = plan.Trials
	resolved.WorkUnits = plan.Trials
	width := uint64(len(plan.Requests))
	if plan.Trials > 0 && plan.Arrangement == ArrangeRotate {
		cycles := plan.Trials / width
		if plan.Trials%width != 0 {
			cycles++
		}
		if cycles > math.MaxUint64/width {
			return nil, errors.New("planned rotation trials overflow")
		}
		resolved.PlannedTrials = cycles * width
		resolved.WorkUnits = cycles
	}
	maxWorkers := uint64(resolved.Workers) //nolint:gosec // Validated positive int worker count.
	if resolved.WorkUnits > 0 && resolved.WorkUnits < maxWorkers {
		resolved.Workers = int(resolved.WorkUnits) //nolint:gosec // Below the validated int worker count.
	}
	resolved.EffectiveConnections = resolved.Workers * resolved.ConnectionsPerWorker
	if resolved.PlannedTrials > math.MaxUint64/width {
		return nil, errors.New("planned request operations overflow")
	}
	resolved.PlannedRequestOperations = resolved.PlannedTrials * width
	resolved.WarmupWidth = len(resolved.Priming)
	workers := uint64(resolved.Workers) //nolint:gosec // Validated positive allocation.
	warmup := uint64(plan.Warmup)
	warmupWidth := uint64(resolved.WarmupWidth) //nolint:gosec // Validated positive width.
	if warmup > math.MaxUint64/workers {
		return nil, errors.New("planned warmup trials overflow")
	}
	resolved.PlannedWarmupTrials = workers * warmup
	if resolved.PlannedWarmupTrials > math.MaxUint64/warmupWidth {
		return nil, errors.New("planned warmup operations overflow")
	}
	resolved.PlannedWarmupOperations = resolved.PlannedWarmupTrials * warmupWidth
	return resolved, nil
}

// ValidateRequest checks materialised destination and framing without traffic.
// It applies the supplied delivery and TLS policies, independently of workers.
func ValidateRequest(request Request, plan Plan) (RequestInfo, error) {
	_, info, _, err := resolveRequest(request, plan)
	if err != nil {
		return info, fmt.Errorf("request %d: %w", request.ID, err)
	}
	return info, nil
}

func resolveRequest(input Request, plan Plan) (Request, RequestInfo, Pool, error) {
	var info RequestInfo
	var pool Pool
	if input.HTTP == nil || input.HTTP.URL == nil {
		return input, info, pool, errors.New("missing request URL")
	}
	request := input.HTTP.Clone(context.Background())
	request.Body, request.GetBody = nil, nil
	request.URL.Scheme = strings.ToLower(request.URL.Scheme)
	parsed, err := url.Parse(request.URL.String())
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return input, info, pool, errors.New("invalid absolute request destination")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return input, info, pool, errors.New("request scheme must be HTTP or HTTPS")
	}
	if input.Protocol != HTTP2 && input.Protocol != HTTP11 {
		return input, info, pool, errors.New("invalid request protocol")
	}
	if input.Protocol == HTTP2 && plan.ReceiveHeaderMax > math.MaxUint32 {
		return input, info, pool, errors.New("HTTP/2 receive header limit exceeds the protocol bound")
	}
	if input.Protocol == HTTP2 && parsed.Scheme != "https" {
		return input, info, pool, errors.New("HTTP/2 requires HTTPS")
	}
	if parsed.Scheme == "http" && (plan.SingleRecord || plan.LastByteSync) {
		return input, info, pool, errors.New("plain HTTP requires multi-record delivery without last-byte synchronisation")
	}
	if parsed.Scheme == "https" && plan.TLSConfig != nil {
		minimum := max(plan.TLSConfig.MinVersion, uint16(tls.VersionTLS12))
		maximum := uint16(tls.VersionTLS13)
		if plan.TLSConfig.MaxVersion != 0 {
			maximum = min(maximum, plan.TLSConfig.MaxVersion)
		}
		if minimum > maximum {
			return input, info, pool, errors.New("TLS configuration excludes supported protocol versions")
		}
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	} else {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return input, info, pool, errors.New("invalid destination port")
		}
		port = strconv.FormatUint(n, 10)
	}
	if request.Method == "" {
		request.Method = http.MethodGet
	}
	input.HTTP, input.Body = request, bytes.Clone(input.Body)
	info = RequestInfo{
		ID: input.ID, Protocol: input.Protocol, Method: request.Method,
		URL: request.URL.String(), BodyBytes: int64(len(input.Body)),
	}
	pool = Pool{
		Scheme: parsed.Scheme, Host: strings.ToLower(parsed.Hostname()),
		Port: port, Address: net.JoinHostPort(strings.ToLower(parsed.Hostname()), port), Protocol: input.Protocol,
	}
	if input.Protocol == HTTP11 {
		prepared, err := prepareH1(request, input.Body, plan.SingleRecord, plan.LastByteSync)
		if err != nil {
			return input, info, pool, err
		}
		var encoded []byte
		for _, record := range prepared.records {
			encoded = append(encoded, record...)
		}
		encoded = append(encoded, prepared.final...)
		decoded, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(encoded)))
		if err != nil {
			return input, info, pool, err
		}
		if decoded.ContentLength >= 0 && (decoded.Header.Get("Content-Length") != "" || decoded.ContentLength > 0) {
			info.DeclaredContentLength = OptionalInt{Value: decoded.ContentLength, Present: true}
		}
		return input, info, pool, nil
	}
	if len(request.Trailer) > 0 {
		return input, info, pool, errors.New("HTTP/2 request trailers are unsupported")
	}
	fields, err := h2RequestFields(input)
	if err != nil {
		return input, info, pool, err
	}
	length, err := h2ValidateRequest(fields)
	if err != nil {
		return input, info, pool, err
	}
	info.DeclaredContentLength = OptionalInt{Value: length, Present: length >= 0}
	info.LengthMismatch = length >= 0 && length != int64(len(input.Body))
	return input, info, pool, nil
}

func h2RequestFields(request Request) ([]hpack.HeaderField, error) {
	if request.HTTP == nil || request.HTTP.URL == nil {
		return nil, errors.New("missing request URL")
	}
	httpRequest := request.HTTP
	header := make(http.Header)
	keys := slices.Sorted(maps.Keys(httpRequest.Header))
	for _, key := range keys {
		if !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid request header name")
		}
		canonical := http.CanonicalHeaderKey(key)
		for _, value := range httpRequest.Header[key] {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, errors.New("invalid request header value")
			}
		}
		header[canonical] = append(header[canonical], httpRequest.Header[key]...)
	}
	if len(header.Values("Transfer-Encoding")) > 0 || len(httpRequest.TransferEncoding) > 0 {
		return nil, errors.New("HTTP/2 request Transfer-Encoding is unsupported")
	}
	if len(header.Values("Upgrade")) > 0 || httpguts.HeaderValuesContainsToken(header.Values("Connection"), "upgrade") {
		return nil, errors.New("upgrades and tunnels are unsupported")
	}
	length, present, err := h1Length(header)
	if err != nil {
		return nil, err
	}
	if present {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	} else if len(request.Body) > 0 {
		header.Set("Content-Length", strconv.Itoa(len(request.Body)))
	}
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			name := http.CanonicalHeaderKey(strings.Trim(token, " \t"))
			if name != "Content-Length" && name != "Te" {
				header.Del(name)
			}
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Http2-Settings"} {
		header.Del(name)
	}
	method, authority := httpRequest.Method, httpRequest.Host
	if method == "" {
		method = http.MethodGet
	}
	if authority == "" {
		authority = httpRequest.URL.Host
	}
	fields := []hpack.HeaderField{
		{Name: ":method", Value: method},
		{Name: ":scheme", Value: httpRequest.URL.Scheme},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: httpRequest.URL.RequestURI()},
	}
	keys = keys[:0]
	for key := range header {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, value := range header[key] {
			fields = append(fields, hpack.HeaderField{Name: strings.ToLower(key), Value: value})
		}
	}
	return fields, nil
}
