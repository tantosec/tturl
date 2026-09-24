package tth2_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

// sendDataField is the body field name into which the fuzz places per-stream
// hash content. tth2's body padding prepends synthetic _padN siblings; this
// named real field survives, so the server can parse the body by Content-Type
// and pull the same bytes back out regardless of padding count.
const sendDataField = "data"

// Bounds on the clamped fuzz inputs. Picked to:
//   - cover narrow and wide batches, including 32- and 64-stream widths,
//     in tth2's progress-aware body scheduler.
//   - reach body sizes large enough to span multiple DATA frames and
//     occasionally drain stream-level flow control (default stream
//     window = 1 MiB; at N=32 streams × 64 KiB ≈ 2 MiB total per Send,
//     drains fire periodically).
//   - cover both sendBodyUnwithheld (withheld == 0) and sendBodyWithTailRelease
//     (withheld > 0) routings.
//   - keep iterations short enough that soak runs accumulate millions
//     of iterations in a day.
const (
	sendMinN         uint16 = 2
	sendMaxN         uint16 = 64
	sendMaxBodyCapKB uint16 = 64
	sendMaxPadScale  uint8  = 32
	sendMaxWithheld  uint8  = 16
)

// sendContentTypes drives the Content-Type fuzz dimension. Each entry builds a
// request body whose Content-Type matches one of tth2's three body-padding
// parsers, so BodyParams padding has a valid grammar to inject into.
var sendContentTypes = [...]struct {
	name  string
	build func(content string) (body io.Reader, contentType string)
}{
	{"JSON", buildJSONPayload},
	{"Form", buildFormPayload},
	{"Multipart", buildMultipartPayload},
}

func buildJSONPayload(content string) (io.Reader, string) {
	payload, err := json.Marshal(map[string]string{sendDataField: content})
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(payload), "application/json"
}

func buildFormPayload(content string) (io.Reader, string) {
	v := url.Values{}
	v.Set(sendDataField, content)
	return strings.NewReader(v.Encode()), "application/x-www-form-urlencoded"
}

func buildMultipartPayload(content string) (io.Reader, string) {
	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField(sendDataField, content); err != nil {
		panic(err)
	}
	if err := mw.Close(); err != nil {
		panic(err)
	}
	return bytes.NewReader(buf.Bytes()), mw.FormDataContentType()
}

// sendFidelityHandler parses the request body by Content-Type, extracts the
// sendDataField value, and emits SHA-256(value) via bodyHashHeader.
// Content-Type-aware extraction lets the same fidelity assertion survive any
// RequestPadding{URLParams, Headers, BodyParams} mix.
var sendFidelityHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	content := extractSendDataField(r.Header.Get("Content-Type"), body)
	sum := sha256.Sum256([]byte(content))
	w.Header().Set(bodyHashHeader, hex.EncodeToString(sum[:]))
})

// extractSendDataField returns the value of sendDataField from body parsed
// according to contentType. Returns "" on parse failure or absence — the
// handler then hashes the empty string, which will mismatch the client's
// expected hash and trip the fuzz assertion.
func extractSendDataField(contentType string, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	switch mediaType {
	case "application/json":
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return ""
		}
		if v, ok := m[sendDataField].(string); ok {
			return v
		}
	case "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			return ""
		}
		return vals.Get(sendDataField)
	case "multipart/form-data":
		boundary, ok := params["boundary"]
		if !ok {
			return ""
		}
		mr := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				return ""
			}
			if part.FormName() == sendDataField {
				data, _ := io.ReadAll(part)
				return string(data)
			}
		}
	}
	return ""
}

// FuzzSend mutates batch width, body format and size, padding, and withholding
// across repeated sends on one client. It checks response success, inverse rank
// mappings, and server-side body digests. Persistent connections also exercise
// HPACK state, stream IDs, flow-control accounting, and stale retries.
func FuzzSend(f *testing.F) {
	addr, tlsCfg := h2test.Serve(f, sendFidelityHandler)

	// Retain one client per fuzz worker to exercise connection state across
	// inputs.
	var (
		clientMu  sync.Mutex
		transport *tth2.Transport
		client    *tth2.Client
	)
	getClient := func() *tth2.Client {
		clientMu.Lock()
		defer clientMu.Unlock()
		if client != nil {
			return client
		}
		transport = &tth2.Transport{TLSClientConfig: tlsCfg}
		client = &tth2.Client{Transport: transport}
		return client
	}
	discardClient := func() {
		clientMu.Lock()
		defer clientMu.Unlock()
		if transport != nil {
			transport.CloseIdleConnections()
			transport = nil
			client = nil
		}
	}
	f.Cleanup(discardClient)

	// Seed corpus: anchor each axis at an interesting value, and cover the key
	// cases (no-pad baseline, narrow and wide batches, zero withholding,
	// max withholding).
	f.Add(uint16(2), uint8(0), int64(0), uint8(0), uint8(1), uint16(0))    // baseline: no padding, no body
	f.Add(uint16(4), uint8(0), int64(1), uint8(8), uint8(1), uint16(4))    // modest padding, JSON, small bodies
	f.Add(uint16(16), uint8(1), int64(2), uint8(32), uint8(1), uint16(16)) // heavy padding, Form
	f.Add(uint16(32), uint8(2), int64(3), uint8(16), uint8(1), uint16(32)) // 32 streams, Multipart
	f.Add(uint16(64), uint8(0), int64(4), uint8(8), uint8(1), uint16(48))  // wide batch, large bodies
	f.Add(uint16(8), uint8(0), int64(5), uint8(4), uint8(0), uint16(16))   // sendBodyUnwithheld (withheld=0)
	f.Add(uint16(8), uint8(1), int64(6), uint8(4), uint8(16), uint16(8))   // max withheld

	f.Fuzz(func(t *testing.T, n uint16, ctSel uint8, seed int64, padMaxScale uint8, withheld uint8, bodyCapKB uint16) {
		// Clamp adversarial fuzz inputs.
		n = min(max(n, sendMinN), sendMaxN)
		ctSel = ctSel % uint8(len(sendContentTypes))
		padScale := min(int(padMaxScale), int(sendMaxPadScale))
		withholdN := min(int(withheld), int(sendMaxWithheld))
		capBytes := min(int(bodyCapKB), int(sendMaxBodyCapKB)) * 1024

		rng := rand.New(rand.NewSource(seed)) //nolint:gosec // math/rand in tests, not security-sensitive
		ct := sendContentTypes[ctSel]

		reqs := make([]*http.Request, n)
		wantHash := make([]string, n)
		for i := range reqs {
			// Per-stream body content is hex-encoded random bytes, length drawn
			// uniformly in [0, capBytes] and rounded down to even so hex encoding is
			// exact. Hex stays valid in JSON strings, URL-encoded form values, and
			// multipart part bodies, so the same content survives all three Content-Type
			// encodings.
			contentLen := rng.Intn(capBytes+1) &^ 1
			buf := make([]byte, contentLen/2)
			_, _ = rng.Read(buf)
			content := hex.EncodeToString(buf)

			body, contentType := ct.build(content)
			req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", contentType)
			reqs[i] = req
			sum := sha256.Sum256([]byte(content))
			wantHash[i] = hex.EncodeToString(sum[:])
		}

		pads := make([]tth2.RequestPadding, n)
		for i := range pads {
			pads[i] = tth2.RequestPadding{
				URLParams:  rng.Intn(padScale + 1),
				Headers:    rng.Intn(padScale + 1),
				BodyParams: rng.Intn(padScale + 1),
			}
		}

		c := getClient()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()

		batch, err := c.SendBatch(ctx, reqs,
			tth2.WithBodyBytesWithheld(withholdN),
			tth2.WithPadding(pads...),
			tth2.WithResponseHeaderCapture(),
		)
		if err != nil {
			t.Fatalf("SendBatch: %v", err)
		}
		results := batch.Results
		if got, want := len(results), len(reqs); got != want {
			t.Fatalf("results count: got %d, want %d", got, want)
		}
		if got, want := len(batch.ArrivalOrder), len(reqs); got != want {
			t.Fatalf("arrival count: got %d, want %d", got, want)
		}
		seen := make([]bool, len(reqs))
		for i, r := range results {
			if !r.Arrived() {
				t.Fatalf("stream %d: result/response = %v", i, r)
			}
			if r.Reset != nil || r.Response.StatusCode != http.StatusOK {
				t.Fatalf("stream %d: reset/status = %v/%d, want nil/200",
					i, r.Reset, r.Response.StatusCode)
			}
			if r.ArrivalRank < 0 || r.ArrivalRank >= len(reqs) {
				t.Fatalf("stream %d: ArrivalRank = %d, want [0,%d)",
					i, r.ArrivalRank, len(reqs))
			}
			if seen[r.ArrivalRank] {
				t.Fatalf("stream %d: duplicate ArrivalRank %d", i, r.ArrivalRank)
			}
			seen[r.ArrivalRank] = true
			if got := r.Response.Header.Get(bodyHashHeader); got != wantHash[i] {
				t.Fatalf("stream %d (ct=%s, withheld=%d, pad=%+v): hash mismatch\n  got:  %s\n  want: %s",
					i, ct.name, withholdN, pads[i], got, wantHash[i])
			}
		}
		for rank, i := range batch.ArrivalOrder {
			if i < 0 || i >= len(results) {
				t.Fatalf("ArrivalOrder[%d] = %d, want a request index", rank, i)
			}
			if results[i].ArrivalRank != rank {
				t.Fatalf("ArrivalOrder[%d] = %d with ArrivalRank %d",
					rank, i, results[i].ArrivalRank)
			}
		}
	})
}
