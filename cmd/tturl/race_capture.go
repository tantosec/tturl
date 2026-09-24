package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

// PaddingRecord is the resolved positional padding recorded for a run.
type PaddingRecord struct {
	// Position is the zero-based stream position.
	Position int
	// URLParams is the number of synthetic query fields.
	URLParams int
	// Headers is the number of synthetic request headers.
	Headers int
	// BodyParams is the number of synthetic body fields.
	BodyParams int
}

// EncodedBody is a complete request body with compact integrity metadata.
type EncodedBody struct {
	// Bytes is the complete body length.
	Bytes int
	// SHA256 is the base64-encoded digest of the complete body.
	SHA256 string
	// EncodedData retains the complete body losslessly.
	EncodedData
}

// HeaderRecord preserves lowercased wire names and repeated values.
type HeaderRecord map[string][]string

// OutcomeRecord is one request's result in a trial, keyed by run-wide request
// ID rather than array order.
type OutcomeRecord struct {
	// Request is the run-wide request index.
	Request int
	// Position is the request's stream position in this trial.
	Position int
	// Rank is nil when no final response headers arrived.
	Rank *int
	// StreamID is nil when no stream was allocated.
	StreamID *uint32
	// Response is nil when final response headers did not arrive.
	Response *ResponseRecord
	// Reset is non-nil when the peer reset the stream.
	Reset *ResetRecord
}

// ResponseRecord is the compact response metadata retained at every capture
// level. The optional payload fields remain nil under metadata capture.
type ResponseRecord struct {
	// Status is the final HTTP status code.
	Status int
	// ContentLength is nil when no unambiguous length was declared.
	ContentLength *int64
	// BodyBytes is the number of response body bytes accepted.
	BodyBytes int64
	// BodyCapturedBytes is the retained body-prefix length.
	BodyCapturedBytes int
	// BodyCapturedComplete reports whether capture covers accepted bytes.
	BodyCapturedComplete bool
	// SHA256 is the base64-encoded digest of all BodyBytes received.
	SHA256 string
	// Extracts contains configured matches in pattern order.
	Extracts []ExtractRecord
	// Headers and Trailers are nil when header capture is disabled.
	Headers  HeaderRecord
	Trailers HeaderRecord
	// Body is nil when response payload output is disabled.
	Body *EncodedData
}

// ExtractRecord carries every match for one configured extraction pattern.
type ExtractRecord struct {
	// Pattern is the configured regular expression.
	Pattern string
	// Selection says whether the expression selects its whole match or its
	// named groups.
	Selection extractionSelection
	// Fields names selected groups in declaration order. It is empty for
	// whole-match selection.
	Fields []string
	// Complete reports that capture covered all accepted response bytes.
	// Matches is then exhaustive.
	Complete bool
	// Matches contains every regex occurrence.
	Matches []ExtractMatch
}

// ExtractMatch is one regex occurrence and its selected values.
type ExtractMatch struct {
	// Offset is the whole match's half-open byte range in the capture.
	Offset [2]int
	// Value is the whole match under whole-match selection.
	Value *EncodedData
	// Captures contains every configured named group under named-group
	// selection, including groups that did not participate in this
	// occurrence.
	Captures []ExtractCapture
}

// ExtractCapture is one named group's value in a regex occurrence.
type ExtractCapture struct {
	Name         string
	Participated bool
	Offset       *[2]int
	Value        *EncodedData
}

// ResetRecord is the stable HTTP/2 reset information attached to one outcome.
type ResetRecord struct {
	// Code is the HTTP/2 error-code name.
	Code string
	// CodeNumber is its numeric value.
	CodeNumber uint32
}

type extractionSelection string

const (
	extractWholeMatch  extractionSelection = "whole_match"
	extractNamedGroups extractionSelection = "named_groups"
)

type extractionField struct {
	name  string
	index int
}

type extractionPattern struct {
	pattern string
	re      *regexp.Regexp
	fields  []extractionField
}

type raceCaptureConfig struct {
	headers   bool
	bodyBytes int64
	extracts  []extractionPattern
}

func (cfg raceCaptureConfig) extractFields() [][]string {
	fields := make([][]string, len(cfg.extracts))
	for i, pattern := range cfg.extracts {
		fields[i] = make([]string, len(pattern.fields))
		for j, field := range pattern.fields {
			fields[i][j] = field.name
		}
	}
	return fields
}

func parseRaceCapture(
	headers bool,
	bodyValue string,
	patterns []string,
	format reportFormat,
) (raceCaptureConfig, error) {
	bodyBytes, err := parseCaptureBodyBytes(bodyValue)
	if err != nil {
		return raceCaptureConfig{}, err
	}
	cfg := raceCaptureConfig{headers: headers, bodyBytes: bodyBytes}
	if headers && format != reportJSON {
		return raceCaptureConfig{}, fmt.Errorf(
			"--capture-headers requires --report json")
	}
	if len(patterns) > 0 && bodyBytes == 0 {
		return raceCaptureConfig{}, fmt.Errorf(
			"--extract-regex requires --capture-body SIZE|unlimited")
	}
	if bodyBytes != 0 && len(patterns) == 0 && format != reportJSON {
		return raceCaptureConfig{}, fmt.Errorf(
			"--capture-body without --extract-regex requires --report json")
	}
	cfg.extracts = make([]extractionPattern, len(patterns))
	for i, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return raceCaptureConfig{}, fmt.Errorf(
				"--extract-regex %q: %s",
				curlblocks.DisplayText(pattern),
				curlblocks.DisplayText(err.Error()))
		}
		fields := make([]extractionField, 0)
		seen := make(map[string]struct{})
		for index, name := range re.SubexpNames() {
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				return raceCaptureConfig{}, fmt.Errorf(
					"--extract-regex %q has more than one group named %q",
					curlblocks.DisplayText(pattern), curlblocks.DisplayText(name))
			}
			seen[name] = struct{}{}
			fields = append(fields, extractionField{name: name, index: index})
		}
		cfg.extracts[i] = extractionPattern{
			pattern: pattern,
			re:      re,
			fields:  fields,
		}
	}
	return cfg, nil
}

func parseCaptureBodyBytes(value string) (int64, error) {
	if value == "" || value == "0" || strings.EqualFold(value, "0B") {
		return 0, nil
	}
	if isUnlimitedFlagValue(value) {
		return tth2.UnlimitedResponseBytes, nil
	}
	lower := strings.ToLower(value)
	multiplier := int64(1)
	number := value
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{
		{suffix: "gib", scale: 1 << 30},
		{suffix: "mib", scale: 1 << 20},
		{suffix: "kib", scale: 1 << 10},
		{suffix: "b", scale: 1},
	} {
		if strings.HasSuffix(lower, unit.suffix) {
			multiplier = unit.scale
			number = value[:len(value)-len(unit.suffix)]
			break
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 ||
		(multiplier > 1 && n > math.MaxInt64/multiplier) {
		return 0, fmt.Errorf(
			"--capture-body must be non-negative bytes with an optional "+
				"B, KiB, MiB, or GiB suffix, or unlimited; got %q",
			curlblocks.DisplayText(value))
	}
	return n * multiplier, nil
}

func (cfg raceCaptureConfig) trialOptions() []tth2.TrialsOption {
	opts := []tth2.TrialsOption{
		tth2.WithResponseBodyCaptureBytes(cfg.bodyBytes),
	}
	if cfg.headers {
		opts = append(opts, tth2.WithResponseHeaderCapture())
	}
	return opts
}

func captureRequestRecords(
	pre *prepared,
	refs runReferences,
	primarySet string,
) ([]RequestRecord, error) {
	reqs := pre.allRequests()
	if len(reqs) != len(refs.refs) {
		return nil, fmt.Errorf(
			"request capture has %d requests but %d references",
			len(reqs), len(refs.refs))
	}
	records := make([]RequestRecord, len(reqs))
	for id, req := range reqs {
		rec, err := captureRequestRecord(req, refs.refs[id])
		if err != nil {
			return nil, fmt.Errorf("capture request %d: %w", id, err)
		}
		if id < pre.measuredCount {
			rec.RequestSet = primarySet
		} else {
			rec.RequestSet = requestSetWarmup
		}
		records[id] = rec
	}
	return records, nil
}

func captureRequestRecord(
	req *http.Request,
	ref runReference,
) (RequestRecord, error) {
	body, hasBody, err := readReusableBody(req)
	if err != nil {
		return RequestRecord{}, err
	}

	pseudo := make(map[string]string, 4)
	for _, field := range tth2.PseudoHeaders(req) {
		pseudo[field.Name] = field.Value
	}
	origin := pseudo[":scheme"] + "://" + pseudo[":authority"]
	rec := RequestRecord{
		Reference:     ref,
		URL:           origin + pseudo[":path"],
		PseudoHeaders: pseudo,
		Headers:       captureWireHeaders(req, int64(len(body))),
	}
	if hasBody {
		rec.Body = &EncodedBody{
			Bytes:       len(body),
			SHA256:      digestBase64(body),
			EncodedData: encodeData(body),
		}
	}
	return rec, nil
}

func readReusableBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, nil
	}
	if req.GetBody == nil {
		return nil, false, fmt.Errorf("body is not reusable")
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil, false, fmt.Errorf("GetBody: %w", err)
	}
	body, readErr := io.ReadAll(rc)
	closeErr := rc.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("read body: %w", readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close body: %w", closeErr)
	}
	return body, true, nil
}

// captureWireHeaders mirrors tth2's request encoder after the pseudo-headers:
// one content-length followed by lowercased regular headers. buildRequest has
// already stripped hop-by-hop fields.
func captureWireHeaders(req *http.Request, bodyLen int64) HeaderRecord {
	headers := make(HeaderRecord)
	if value := req.Header.Get("Content-Length"); value != "" {
		headers["content-length"] = []string{value}
	} else {
		headers["content-length"] = []string{strconv.FormatInt(bodyLen, 10)}
	}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "host" {
			continue
		}
		headers[lower] = append(headers[lower], values...)
	}
	return headers
}

func captureTrialRecord(
	trial *tth2.Trial,
	requestOffset int,
	cfg raceCaptureConfig,
) (TrialRecord, error) {
	rec := TrialRecord{
		Trial:         trial,
		RequestOffset: requestOffset,
		Outcomes:      make([]OutcomeRecord, len(trial.Arrangement)),
	}
	positions := make([]int, len(trial.Arrangement))
	for position, localID := range trial.Arrangement {
		if localID >= 0 && localID < len(positions) {
			positions[localID] = position
		}
	}
	for localID := range rec.Outcomes {
		outcome := OutcomeRecord{
			Request:  requestOffset + localID,
			Position: positions[localID],
		}
		if localID < len(trial.Results) {
			result := trial.Results[localID]
			streamID := result.StreamID
			outcome.StreamID = &streamID
			if result.ArrivalRank >= 0 {
				rank := result.ArrivalRank
				outcome.Rank = &rank
			}
			if result.Arrived() {
				body := result.Response.Body.Captured
				response := &ResponseRecord{
					Status:               result.Response.StatusCode,
					BodyBytes:            result.Response.Body.BytesReceived,
					BodyCapturedBytes:    len(body),
					BodyCapturedComplete: result.Response.Body.CapturedComplete(),
					SHA256: base64.StdEncoding.EncodeToString(
						result.Response.Body.SHA256[:]),
					Extracts: captureExtracts(
						body, cfg.extracts,
						result.Response.Body.CapturedComplete()),
				}
				if result.Response.ContentLength >= 0 {
					contentLength := result.Response.ContentLength
					response.ContentLength = &contentLength
				}
				if cfg.headers {
					response.Headers = captureResponseHeaders(
						result.Response.Header)
					response.Trailers = captureResponseHeaders(
						result.Response.Trailer)
				}
				if cfg.bodyBytes != 0 {
					encoded := EncodedData{
						Encoding: "base64",
						Data:     base64.StdEncoding.EncodeToString(body),
					}
					response.Body = &encoded
				}
				outcome.Response = response
			}
			if result.Reset != nil {
				outcome.Reset = &ResetRecord{
					Code:       result.Reset.Code.String(),
					CodeNumber: uint32(result.Reset.Code),
				}
			}
		}
		rec.Outcomes[localID] = outcome
	}
	return rec, nil
}

func captureExtracts(
	body []byte,
	patterns []extractionPattern,
	complete bool,
) []ExtractRecord {
	extracts := make([]ExtractRecord, len(patterns))
	for i, pattern := range patterns {
		selection := extractWholeMatch
		fields := make([]string, len(pattern.fields))
		if len(pattern.fields) > 0 {
			selection = extractNamedGroups
		}
		for j, field := range pattern.fields {
			fields[j] = field.name
		}
		extract := ExtractRecord{
			Pattern: pattern.pattern, Selection: selection,
			Fields: fields, Complete: complete, Matches: []ExtractMatch{},
		}
		for _, indexes := range pattern.re.FindAllSubmatchIndex(body, -1) {
			start, end := indexes[0], indexes[1]
			match := ExtractMatch{Offset: [2]int{start, end}}
			if len(pattern.fields) == 0 {
				value := encodeData(body[start:end])
				match.Value = &value
			} else {
				match.Captures = make([]ExtractCapture, len(pattern.fields))
				for j, field := range pattern.fields {
					capture := ExtractCapture{Name: field.name}
					captureStart := indexes[2*field.index]
					captureEnd := indexes[2*field.index+1]
					if captureStart >= 0 {
						capture.Participated = true
						offset := [2]int{captureStart, captureEnd}
						value := encodeData(body[captureStart:captureEnd])
						capture.Offset = &offset
						capture.Value = &value
					}
					match.Captures[j] = capture
				}
			}
			extract.Matches = append(extract.Matches, match)
		}
		extracts[i] = extract
	}
	return extracts
}

func captureResponseHeaders(headers http.Header) HeaderRecord {
	record := make(HeaderRecord, len(headers))
	for name, values := range headers {
		lower := strings.ToLower(name)
		record[lower] = append([]string(nil), values...)
	}
	return record
}

func resolvedPadding(pads []tth2.RequestPadding, count int) []PaddingRecord {
	records := make([]PaddingRecord, count)
	for position := range count {
		records[position].Position = position
		if position >= len(pads) {
			continue
		}
		records[position].URLParams = pads[position].URLParams
		records[position].Headers = pads[position].Headers
		records[position].BodyParams = pads[position].BodyParams
	}
	return records
}

func commonWireOrigin(reqs []*http.Request) *string {
	if len(reqs) == 0 {
		return nil
	}
	origin, _ := wireTarget(reqs[0])
	for _, req := range reqs[1:] {
		if candidate, _ := wireTarget(req); candidate != origin {
			return nil
		}
	}
	return &origin
}

const (
	requestSetMeasured = "measured"
	requestSetTrial    = "trial"
	requestSetWarmup   = "warmup"
)
