package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"unicode/utf16"
	"unicode/utf8"
)

// wireString is a byte-bearing JSON string. Valid UTF-8 stays a string; invalid
// UTF-8 and literals colliding with the reserved marker use injective base64.
type wireString string

func (s wireString) MarshalJSON() ([]byte, error) {
	raw := string(s)
	if !utf8.ValidString(raw) || canonicalBase64Marker(raw) {
		raw = "$B64[" + base64.StdEncoding.EncodeToString([]byte(raw)) + "]"
	}
	return appendASCIIJSONString(nil, raw), nil
}

// encodedString is data whose explicit encoding field already disambiguates
// UTF-8 from base64. It needs ASCII JSON escaping but no reserved marker.
type encodedString string

func (s encodedString) MarshalJSON() ([]byte, error) {
	return appendASCIIJSONString(nil, string(s)), nil
}

func canonicalBase64Marker(s string) bool {
	const prefix = "$B64["
	if len(s) < len(prefix)+1 || s[:len(prefix)] != prefix ||
		s[len(s)-1] != ']' {
		return false
	}
	payload := s[len(prefix) : len(s)-1]
	decoded, err := base64.StdEncoding.DecodeString(payload)
	return err == nil &&
		base64.StdEncoding.EncodeToString(decoded) == payload
}

func appendASCIIJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			dst = append(dst, '\\', byte(r))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				dst = append(dst, byte(r))
			case r <= 0xffff:
				dst = appendUnicodeEscape(dst, r)
			default:
				high, low := utf16.EncodeRune(r)
				dst = appendUnicodeEscape(dst, high)
				dst = appendUnicodeEscape(dst, low)
			}
		}
	}
	return append(dst, '"')
}

func appendUnicodeEscape(dst []byte, r rune) []byte {
	const hex = "0123456789abcdef"
	return append(dst,
		'\\', 'u',
		hex[r>>12],
		hex[(r>>8)&0xf],
		hex[(r>>4)&0xf],
		hex[r&0xf],
	)
}

// wireStringMap and wireHeaderMap preserve arbitrary bytes in object keys as
// well as values. Their key order is deterministic.
type (
	wireStringMap map[string]string
	wireHeaderMap map[string][]string
)

func (m wireStringMap) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(appendASCIIJSONString(nil, encodedWireString(key)))
		out.WriteByte(':')
		out.Write(appendASCIIJSONString(nil, encodedWireString(m[key])))
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func (m wireHeaderMap) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(appendASCIIJSONString(nil, encodedWireString(key)))
		out.WriteString(":[")
		for j, value := range m[key] {
			if j > 0 {
				out.WriteByte(',')
			}
			out.Write(appendASCIIJSONString(nil, encodedWireString(value)))
		}
		out.WriteByte(']')
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func encodedWireString(s string) string {
	if !utf8.ValidString(s) || canonicalBase64Marker(s) {
		return "$B64[" + base64.StdEncoding.EncodeToString([]byte(s)) + "]"
	}
	return s
}

func wireStrings(values []string) []wireString {
	converted := make([]wireString, len(values))
	for i, value := range values {
		converted[i] = wireString(value)
	}
	return converted
}

func wireStringPointer(value *string) *wireString {
	if value == nil {
		return nil
	}
	converted := wireString(*value)
	return &converted
}

var (
	_ json.Marshaler = wireString("")
	_ json.Marshaler = encodedString("")
	_ json.Marshaler = wireStringMap(nil)
	_ json.Marshaler = wireHeaderMap(nil)
)

// UnmarshalJSON restores one layer of the injective byte-string encoding.
func (s *wireString) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = wireString(decodedWireString(value))
	return nil
}

func decodedWireString(value string) string {
	if !canonicalBase64Marker(value) {
		return value
	}
	const prefix = "$B64["
	decoded, _ := base64.StdEncoding.DecodeString(value[len(prefix) : len(value)-1])
	return string(decoded)
}

func (m *wireStringMap) UnmarshalJSON(data []byte) error {
	var encoded map[string]string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	decoded := make(wireStringMap, len(encoded))
	for key, value := range encoded {
		decoded[decodedWireString(key)] = decodedWireString(value)
	}
	*m = decoded
	return nil
}

func (m *wireHeaderMap) UnmarshalJSON(data []byte) error {
	var encoded map[string][]string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	decoded := make(wireHeaderMap, len(encoded))
	for key, values := range encoded {
		restored := make([]string, len(values))
		for i, value := range values {
			restored[i] = decodedWireString(value)
		}
		decoded[decodedWireString(key)] = restored
	}
	*m = decoded
	return nil
}

// EncodedData carries arbitrary bytes without imposing base64 on ordinary
// UTF-8 text.
type EncodedData struct {
	// Encoding is "utf-8" or "base64".
	Encoding string
	// Data is text in Encoding.
	Data string
}

func encodeData(data []byte) EncodedData {
	if utf8.Valid(data) {
		return EncodedData{Encoding: "utf-8", Data: string(data)}
	}
	return EncodedData{
		Encoding: "base64",
		Data:     base64.StdEncoding.EncodeToString(data),
	}
}

func digestBase64(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}
