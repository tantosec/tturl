package curlblocks

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// encoder is one value transform in a --vary encoder chain.
type encoder int

const (
	encHex      encoder = iota // lower-case hex of the raw bytes
	encHexUpper                // upper-case hex of the raw bytes
	encB64                     // standard (padded) base64
	encB64URL                  // URL-safe base64, no padding (RFC 4648 base64url)
	encURL                     // RFC 3986 percent-encoding (see urlEncode)
)

// parseEncoders parses the ':'-separated encoder chain from a --vary head
// (the text after NAME). An empty chain (no ':') yields no encoders.
func parseEncoders(chain string) ([]encoder, error) {
	if chain == "" {
		return nil, nil
	}
	names := strings.Split(chain, ":")
	encoders := make([]encoder, len(names))
	for i, n := range names {
		switch n {
		case "hex":
			encoders[i] = encHex
		case "HEX":
			encoders[i] = encHexUpper
		case "b64":
			encoders[i] = encB64
		case "b64url":
			encoders[i] = encB64URL
		case "url":
			encoders[i] = encURL
		default:
			return nil, fmt.Errorf("unknown encoder %q (want hex, HEX, b64, b64url, or url)", n)
		}
	}
	return encoders, nil
}

// applyEncoders runs raw through the encoder chain in order, so "hex:b64" is
// the base64 of the hex text.
func applyEncoders(encoders []encoder, raw string) string {
	s := raw
	for _, e := range encoders {
		switch e {
		case encHex:
			s = hex.EncodeToString([]byte(s))
		case encHexUpper:
			s = strings.ToUpper(hex.EncodeToString([]byte(s)))
		case encB64:
			s = base64.StdEncoding.EncodeToString([]byte(s))
		case encB64URL:
			s = base64.RawURLEncoding.EncodeToString([]byte(s))
		case encURL:
			s = urlEncode([]byte(s))
		}
	}
	return s
}

// DisplayText renders s as printable ASCII using the same $HEX[...] form as
// [Label]. Printable ASCII is unchanged. Consumers should use it before
// matching or displaying user text alongside labels.
//
// Rendering is one-way and not injective: literal $HEX[...] text can equal the
// rendering of different bytes. [ResolveNames] keeps labels unique, but a
// display key can therefore select both values.
func DisplayText(s string) string { return displayValue(s) }

// displayValue renders raw for a display label: a value that is not clean
// printable ASCII (a control or high-bit byte, or edge whitespace that would be
// lost) becomes recognisable hashcat-style $HEX[<hex>] text; otherwise raw is
// shown as-is. DisplayText is the exported form.
func displayValue(raw string) string {
	if needsHexDisplay(raw) {
		return "$HEX[" + hex.EncodeToString([]byte(raw)) + "]"
	}
	return raw
}

// needsHexDisplay reports whether raw cannot be shown verbatim in a label: it
// holds a byte outside the printable ASCII range 0x20..0x7e, or it has leading
// or trailing whitespace (which a bare rendering would silently drop). An empty
// value is shown as an empty value, not $HEX[].
func needsHexDisplay(raw string) bool {
	if raw == "" {
		return false
	}
	if edge := raw[0]; edge == ' ' || edge == '\t' {
		return true
	}
	if edge := raw[len(raw)-1]; edge == ' ' || edge == '\t' {
		return true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return true
		}
	}
	return false
}
