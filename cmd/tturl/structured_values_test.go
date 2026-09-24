package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"slices"
	"time"
	"unicode/utf8"
)

// decodeStructuredData checks the explicit byte encoding without interpreting
// the separate wire-string marker convention.
func decodeStructuredData(encoding, data string) ([]byte, error) {
	switch encoding {
	case "utf-8":
		if !utf8.ValidString(data) {
			return nil, fmt.Errorf("structured data is not valid UTF-8")
		}
		return []byte(data), nil
	case "base64":
		decoded, err := base64.StdEncoding.Strict().DecodeString(data)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != data {
			return nil, fmt.Errorf("structured data is not canonical base64")
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("structured data has an unknown encoding")
	}
}

func validateStructuredResponse(response structuredResponse, capture structuredCapture) ([]byte, error) {
	if response.BodyBytes < 0 || response.BodyCapturedBytes < 0 || int64(response.BodyCapturedBytes) > response.BodyBytes {
		return nil, fmt.Errorf("response capture exceeds accepted bytes")
	}
	if response.BodyCapturedComplete != (int64(response.BodyCapturedBytes) == response.BodyBytes) {
		return nil, fmt.Errorf("response capture completeness disagrees with accepted bytes")
	}
	digest, err := decodeStructuredData("base64", response.SHA256)
	if err != nil || len(digest) != 32 {
		return nil, fmt.Errorf("response digest must encode 32 SHA-256 bytes")
	}
	var data []byte
	if response.Body != nil {
		if response.Body.Encoding != "base64" {
			return nil, fmt.Errorf("captured response data must use base64")
		}
		var err error
		data, err = decodeStructuredData(response.Body.Encoding, string(response.Body.Data))
		if err != nil {
			return nil, err
		}
	}
	if len(data) != response.BodyCapturedBytes {
		return nil, fmt.Errorf("response capture length disagrees with decoded data")
	}
	if response.BodyCapturedComplete && digestBase64(data) != response.SHA256 {
		return nil, fmt.Errorf("response digest disagrees with complete captured data")
	}
	if capture.BodyBytes != nil && (int64(len(data)) > *capture.BodyBytes ||
		(*capture.BodyBytes == 0 && response.Body != nil)) {
		return nil, fmt.Errorf("response data disagrees with capture limit")
	}
	if !capture.Headers && (response.Headers != nil || response.Trailers != nil) {
		return nil, fmt.Errorf("response headers disagree with capture configuration")
	}
	return data, nil
}

func validateStructuredExtracts(
	response jsonRaceResponse, capture structuredCapture, definitions []jsonRaceExtractor,
) error {
	data, err := validateStructuredResponse(response.structuredResponse, capture)
	if err != nil {
		return err
	}
	patterns := make([]string, len(definitions))
	for i, definition := range definitions {
		if definition.ExtractorID != i {
			return fmt.Errorf("extractor IDs must be dense and ordered")
		}
		patterns[i] = string(definition.Pattern)
	}
	config, err := parseRaceCapture(false, "unlimited", patterns, reportJSON)
	if err != nil {
		return fmt.Errorf("invalid structured extractor configuration")
	}
	projector := jsonRaceReporter{extractors: definitions}
	expected, err := projector.projectExtracts(captureExtracts(data, config.extracts, response.BodyCapturedComplete))
	if err != nil {
		return err
	}
	if len(response.Extracts) != len(expected) {
		return fmt.Errorf("response extraction catalogue is incomplete")
	}
	for i, actual := range response.Extracts {
		want := expected[i]
		if actual.ExtractorID != want.ExtractorID || actual.Complete != want.Complete ||
			len(actual.Matches) != len(want.Matches) {
			return fmt.Errorf("response extraction disagrees with retained bytes")
		}
		for j, match := range actual.Matches {
			wm := want.Matches[j]
			if match.Offset != wm.Offset || len(match.Captures) != len(wm.Captures) ||
				!equalStructuredExtractData(match.Encoding, match.Data, wm.Encoding, wm.Data) {
				return fmt.Errorf("extracted match disagrees with retained bytes")
			}
			for k, field := range match.Captures {
				wf := wm.Captures[k]
				if field.Name != wf.Name || field.Participated != wf.Participated ||
					!equalStructuredOffset(field.Offset, wf.Offset) ||
					!equalStructuredExtractData(field.Encoding, field.Data, wf.Encoding, wf.Data) {
					return fmt.Errorf("extracted capture disagrees with retained bytes")
				}
			}
		}
	}
	return nil
}

func equalStructuredOffset(left, right *[2]int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func equalStructuredExtractData(le *string, ld *encodedString, re *string, rd *encodedString) bool {
	if le == nil || ld == nil || re == nil || rd == nil {
		return le == re && ld == rd
	}
	left, err := decodeStructuredData(*le, string(*ld))
	if err != nil {
		return false
	}
	right, err := decodeStructuredData(*re, string(*rd))
	return err == nil && bytes.Equal(left, right)
}

// validateStructuredRequest checks identity and any fully recorded body bytes.
func validateStructuredRequest(request structuredRequest, labels map[string]bool) error {
	if request.Label == "" || labels[request.Label] {
		return fmt.Errorf("request labels must be non-empty and unique")
	}
	for _, r := range request.Label {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("request label must be printable ASCII")
		}
	}
	if body := request.Body; body != nil {
		data, err := decodeStructuredData(body.Encoding, string(body.Data))
		if err != nil {
			return err
		}
		if body.Bytes != len(data) || body.SHA256 != digestBase64(data) {
			return fmt.Errorf("request body length or digest disagrees with its bytes")
		}
	}
	labels[request.Label] = true
	return nil
}

// validateStructuredDelivery verifies capacity and the composed rate arithmetic.
func validateStructuredDelivery(d structuredDelivery, width int) error {
	if width < 1 || d.ConnectionLimit < 1 || d.ConnectionLimit > d.ConnectionCeiling {
		return fmt.Errorf("effective connections must be positive and within the ceiling")
	}
	if d.Selection.Mode == "default" || d.Selection.State == "skipped_single_trial" {
		if d.ConnectionCeiling != 1 || d.ConnectionLimit != 1 {
			return fmt.Errorf("default and single-trial delivery have capacity one")
		}
	}
	if d.Selection.Mode == "fixed" && (d.Selection.RequestedConnections == nil ||
		d.ConnectionCeiling != *d.Selection.RequestedConnections) {
		return fmt.Errorf("fixed ceiling differs from requested capacity")
	}
	if r := d.RateLimits; r != nil {
		var interval int64
		for _, rate := range []*float64{r.BatchMaxPerSecond, r.RequestMaxPerSecond} {
			if rate != nil && (*rate <= 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0)) {
				return fmt.Errorf("delivery rates must be positive and finite")
			}
		}
		if r.BatchMaxPerSecond != nil {
			interval = int64(math.Ceil(1e9 / *r.BatchMaxPerSecond))
		}
		if r.RequestMaxPerSecond != nil {
			interval = max(interval, int64(math.Ceil(float64(width)*1e9 / *r.RequestMaxPerSecond)))
		}
		if interval < 1 || r.EffectiveBatchIntervalNS != interval {
			return fmt.Errorf("reported measured interval differs from composed rates")
		}
	}
	if fit := d.PathFit; fit != nil {
		if len(fit.RTTSamplesNS) != 3 || slices.Min(fit.RTTSamplesNS) <= 0 ||
			fit.RTTBasisNS != slices.Max(fit.RTTSamplesNS) {
			return fmt.Errorf("RTT basis must be the maximum of three positive samples")
		}
		if d.RateLimits == nil {
			return fmt.Errorf("fitted delivery requires a finite rate")
		}
		connections, err := fittedConnectionCount(time.Duration(fit.RTTBasisNS),
			time.Duration(d.RateLimits.EffectiveBatchIntervalNS))
		if err != nil || connections != fit.FittedConnections {
			return fmt.Errorf("fitted count differs from RTT/interval with headroom")
		}
		ceiling := connections
		if d.Selection.Maximum == nil {
			return fmt.Errorf("fitted delivery requires a maximum policy")
		}
		if d.Selection.Maximum.Mode == "bounded" {
			if d.Selection.Maximum.Connections == nil || *d.Selection.Maximum.Connections < 1 {
				return fmt.Errorf("bounded fitting requires a positive maximum")
			}
			ceiling = min(ceiling, *d.Selection.Maximum.Connections)
		}
		if ceiling != d.ConnectionCeiling || fit.Capped != (ceiling < connections) {
			return fmt.Errorf("fit cap and ceiling must agree with the maximum policy")
		}
	}
	return nil
}
