package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/net/http2"
)

var demoServerTestTime = time.Unix(123, 456)

func TestParseDemoServerArgs(t *testing.T) {
	t.Parallel()
	cfg, help, err := parseDemoServerArgs(nil)
	if err != nil || help {
		t.Fatalf("parseDemoServerArgs(nil) = %+v, %t, %v", cfg, help, err)
	}
	if cfg.port != defaultDemoServerPort ||
		cfg.bindHost != defaultDemoServerBindHost ||
		cfg.timingWorkMax.maximum != defaultDemoServerTimingWorkMax ||
		cfg.timingWorkMax.unlimited ||
		cfg.concurrentMax != defaultDemoServerConcurrentMax ||
		cfg.quotaWorkTarget != defaultDemoQuotaWork ||
		cfg.network.rtt != 0 || cfg.network.jitter != 0 {
		t.Errorf("default config = %+v", cfg)
	}

	cfg, help, err = parseDemoServerArgs([]string{
		"-p", "0", "--bind-host", "::1", "--timing-work-max", "2.5s",
		"--max-concurrent-streams", "7",
		"--quota-work", "75us", "--rtt", "200ms", "--jitter", "20ms",
	})
	if err != nil || help {
		t.Fatalf("parseDemoServerArgs(custom) = %+v, %t, %v", cfg, help, err)
	}
	if cfg.port != 0 || cfg.bindHost != "::1" ||
		cfg.timingWorkMax.maximum != 2500*time.Millisecond ||
		cfg.timingWorkMax.unlimited || cfg.concurrentMax != 7 ||
		cfg.quotaWorkTarget != 75*time.Microsecond ||
		cfg.network.rtt != 200*time.Millisecond ||
		cfg.network.jitter != 20*time.Millisecond {
		t.Errorf("custom config = %+v", cfg)
	}

	cfg, help, err = parseDemoServerArgs([]string{
		"--port", "9443", "-b", "localhost", "--timing-work-max", "0",
		"--quota-work", "0",
	})
	if err != nil || help || cfg.port != 9443 ||
		cfg.bindHost != "localhost" || !cfg.timingWorkMax.disabled() ||
		cfg.quotaWorkTarget != 0 {
		t.Errorf("alternate config = %+v, %t, %v", cfg, help, err)
	}

	cfg, help, err = parseDemoServerArgs([]string{
		"--timing-work-max", "UNLIMITED",
	})
	if err != nil || help || !cfg.timingWorkMax.unlimited ||
		cfg.timingWorkMax.maximum != 0 {
		t.Errorf("unlimited config = %+v, %t, %v", cfg, help, err)
	}
}

func TestParseDemoServerArgsHelp(t *testing.T) {
	t.Parallel()
	for _, option := range []string{"-h", "--help"} {
		_, help, err := parseDemoServerArgs([]string{option})
		if err != nil || !help {
			t.Errorf("parseDemoServerArgs(%q) = help %t, error %v",
				option, help, err)
		}
	}
}

func TestParseDemoServerArgsErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"--unknown"}, want: "unknown flag"},
		{name: "argument", args: []string{"extra"}, want: "unexpected argument"},
		{name: "negative port", args: []string{"-p", "-1"}, want: "'--port' must be"},
		{name: "large port", args: []string{"-p", "65536"}, want: "'--port' must be"},
		{name: "negative timing work", args: []string{"--timing-work-max", "-1s"}, want: "'--timing-work-max' must"},
		{name: "invalid timing work", args: []string{"--timing-work-max", "forever"}, want: "Go duration or unlimited"},
		{name: "negative quota work", args: []string{"--quota-work", "-1ns"}, want: "'--quota-work' must"},
		{name: "negative rtt", args: []string{"--rtt", "-1ms"}, want: "'--rtt' must"},
		{name: "negative jitter", args: []string{"--jitter", "-1ms"}, want: "'--jitter' must"},
		{name: "zero streams", args: []string{"--max-concurrent-streams", "0"}, want: "'--max-concurrent-streams' must"},
		{
			name: "large streams",
			args: []string{"--max-concurrent-streams", "4294967296"},
			want: "'--max-concurrent-streams' must",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := parseDemoServerArgs(test.args); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Errorf("parseDemoServerArgs(%q) error = %v, want %q",
					test.args, err, test.want)
			}
		})
	}
}

func TestDemoServerUsageContract(t *testing.T) {
	t.Parallel()
	usage := renderDemoServerUsage(commandByID(commandDemoServer))
	if !strings.Contains(usage, "--quota-work DURATION") ||
		!strings.Contains(usage, "--timing-work-max DURATION|unlimited") ||
		!strings.Contains(usage, "default 25us; 0 = disabled") ||
		!strings.Contains(usage, "default 1s; 0 disables routes") {
		t.Errorf("usage has the wrong timing-work contract:\n%s", usage)
	}
	if !strings.HasSuffix(usage, "\n") || strings.HasSuffix(usage, "\n\n") {
		t.Errorf("usage has an invalid document boundary: %q", usage)
	}
	for line := range strings.SplitSeq(usage, "\n") {
		if len(line) > textWidth {
			t.Errorf("usage has a %d-column line: %q", len(line), line)
		}
		for _, b := range []byte(line) {
			if b < 0x20 || b > 0x7e {
				t.Errorf("usage contains non-printable byte 0x%02x", b)
			}
		}
	}
}

func TestDemoServerRoutes(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	effective := demoServerEffectiveConfig{settings: cfg}
	mux := newDemoServerMuxAt(effective, func() time.Time {
		return demoServerTestTime
	})

	t.Run("index", func(t *testing.T) {
		response := serveDemoRequest(mux, http.MethodGet, "/")
		if response.Code != http.StatusOK || response.Body.String() != demoServerRouteIndex {
			t.Errorf("GET / = %d, %q", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		assertNoDemoServerTimestamps(t, response)
	})

	t.Run("sleep", func(t *testing.T) {
		response := serveDemoRequest(mux, http.MethodGet,
			"/sleep?duration=0%73&padding=one&padding=two&empty=")
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Errorf("GET /sleep = %d, %q", response.Code, response.Body.String())
		}
		assertDemoServerTimestamps(t, response,
			"Handler-Start", "Sleep-Start", "Sleep-End")
	})

	t.Run("sleep form", func(t *testing.T) {
		response := serveDemoFormRequest(
			mux, "/sleep", "duration=0&filler=available")
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Errorf("POST /sleep = %d, %q",
				response.Code, response.Body.String())
		}
		assertDemoServerTimestamps(t, response,
			"Handler-Start", "Sleep-Start", "Sleep-End")
	})

	t.Run("work", func(t *testing.T) {
		response := serveDemoRequest(mux, http.MethodGet, "/work?iterations=3")
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Errorf("GET /work = %d, %q", response.Code, response.Body.String())
		}
		assertDemoServerTimestamps(t, response,
			"Handler-Start", "Work-Start", "Work-End")
	})

	t.Run("work form", func(t *testing.T) {
		response := serveDemoFormRequest(
			mux, "/work", "iterations=3&filler=available")
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Errorf("POST /work = %d, %q",
				response.Code, response.Body.String())
		}
		assertDemoServerTimestamps(t, response,
			"Handler-Start", "Work-Start", "Work-End")
	})

	t.Run("sha256", func(t *testing.T) {
		body := strings.Repeat("hello\x00world", demoServerHashReadBufferSize)
		want := sha256.Sum256([]byte(body))
		response := serveDemoBodyRequest(
			mux, http.MethodPost, "/sha256", body, "application/octet-stream")
		if response.Code != http.StatusOK ||
			response.Body.String() != hex.EncodeToString(want[:]) {
			t.Errorf("POST /sha256 = %d, %q",
				response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
		assertDemoServerTimestamps(t, response,
			"Handler-Start", "Hash-Start", "Hash-End")
	})

	t.Run("sha256 read error", func(t *testing.T) {
		request := httptest.NewRequestWithContext(
			t.Context(), http.MethodPost, "/sha256", nil)
		request.Body = io.NopCloser(iotest.ErrReader(errors.New("body failed")))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(),
				"could not read complete request body: body failed") {
			t.Errorf("POST /sha256 read error = %d, %q",
				response.Code, response.Body.String())
		}
	})

	t.Run("now", func(t *testing.T) {
		response := serveDemoRequest(mux, http.MethodGet, "/now")
		got, err := strconv.ParseInt(response.Body.String(), 10, 64)
		want := demoServerTestTime.UnixNano()
		if err != nil || response.Code != http.StatusOK || got != want {
			t.Errorf("GET /now = %d, %q, parse %v; want %d",
				response.Code, response.Body.String(), err, want)
		}
		assertNoDemoServerTimestamps(t, response)
	})

	t.Run("method", func(t *testing.T) {
		if got := serveDemoRequest(mux, http.MethodPut,
			"/sleep?duration=0").Code; got != http.StatusMethodNotAllowed {
			t.Errorf("PUT /sleep status = %d, want 405", got)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		if got := serveDemoRequest(mux, http.MethodGet, "/missing").Code; got != http.StatusNotFound {
			t.Errorf("GET /missing status = %d, want 404", got)
		}
	})
}

func TestDemoServerRouteValidation(t *testing.T) {
	t.Parallel()
	mux := newDemoServerMux(demoServerEffectiveConfig{
		settings: demoServerConfig{timingWorkMax: demoTimingWorkLimit{
			maximum: time.Second,
		}},
		workMaxIterations: 100,
	})
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/sleep", want: "exactly one duration"},
		{path: "/sleep?duration=", want: "exactly one duration"},
		{path: "/sleep?duration=1ms&duration=2ms", want: "exactly one duration"},
		{path: "/sleep?duration=later", want: "non-negative Go duration"},
		{path: "/sleep?duration=-1ns", want: "non-negative Go duration"},
		{path: "/sleep?duration=1001ms", want: "server limit of 1s"},
		{path: "/work", want: "exactly one iterations"},
		{path: "/work?iterations=%", want: "invalid URL query"},
		{path: "/work?iterations=-1", want: "unsigned decimal integer"},
		{path: "/work?iterations=101", want: "server limit of 100"},
	} {
		response := serveDemoRequest(mux, http.MethodGet, test.path)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), test.want) {
			t.Errorf("GET %s = %d, %q; want 400 containing %q",
				test.path, response.Code, response.Body.String(), test.want)
		}
	}
}

func TestDemoServerFormValidation(t *testing.T) {
	t.Parallel()
	mux := newDemoServerMux(
		demoServerEffectiveConfig{
			settings: demoServerConfig{timingWorkMax: demoTimingWorkLimit{
				maximum: time.Second,
			}},
			workMaxIterations: 100,
		})
	for _, test := range []struct {
		name string
		path string
		body string
		want string
	}{
		{name: "sleep missing", path: "/sleep", want: "exactly one duration"},
		{name: "sleep empty", path: "/sleep", body: "duration=", want: "exactly one duration"},
		{
			name: "sleep duplicate", path: "/sleep",
			body: "duration=1ms&duration=2ms", want: "exactly one duration",
		},
		{name: "sleep invalid", path: "/sleep", body: "duration=later", want: "non-negative Go duration"},
		{name: "sleep over limit", path: "/sleep", body: "duration=1001ms", want: "server limit of 1s"},
		{name: "work missing", path: "/work", want: "exactly one iterations"},
		{name: "work invalid", path: "/work", body: "iterations=-1", want: "unsigned decimal integer"},
		{name: "work over limit", path: "/work", body: "iterations=101", want: "server limit of 100"},
		{name: "malformed encoding", path: "/work", body: "iterations=%", want: "invalid URL-encoded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := serveDemoFormRequest(mux, test.path, test.body)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), test.want) {
				t.Errorf("POST %s body %q = %d, %q; want 400 containing %q",
					test.path, test.body, response.Code,
					response.Body.String(), test.want)
			}
		})
	}
}

func TestDemoServerDisabledTimingWorkRoutes(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.timingWorkMax = demoTimingWorkLimit{}
	mux := newDemoServerMux(demoServerEffectiveConfig{
		settings: cfg, quotaWorkIterations: 1,
	})
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "get sleep", method: http.MethodGet, path: "/sleep?duration=later"},
		{name: "post sleep", method: http.MethodPost, path: "/sleep", body: "%"},
		{name: "get work", method: http.MethodGet, path: "/work?iterations=-1"},
		{name: "post work", method: http.MethodPost, path: "/work", body: "%"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := serveDemoBodyRequest(
				mux, test.method, test.path, test.body,
				"application/x-www-form-urlencoded")
			if response.Code != http.StatusForbidden || response.Body.String() !=
				"timing work is disabled by server configuration\n" {
				t.Errorf("%s %s = %d, %q; want 403 disabled response",
					test.method, test.path, response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			assertNoDemoServerTimestamps(t, response)
		})
	}

	created := serveDemoRequest(mux, http.MethodPost, "/quotas")
	var creation struct {
		Result demoQuotaCreateResult `json:"result"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &creation); err != nil {
		t.Fatalf("decode quota creation: %v", err)
	}
	used := serveDemoRequest(mux, http.MethodPost,
		"/quotas/"+creation.Result.Token+"/use")
	if created.Code != http.StatusCreated || used.Code != http.StatusOK {
		t.Errorf("disabled timing work changed quota routes: create %d, use %d",
			created.Code, used.Code)
	}
}

func TestDemoServerUnlimitedRouteConfiguration(t *testing.T) {
	t.Parallel()
	if got, err := parseDemoSleepValue("24h", 0); err != nil || got != 24*time.Hour {
		t.Errorf("parseDemoSleepValue unlimited = %v, %v", got, err)
	}
	if got, err := parseDemoWorkValue("18446744073709551615", 0); err != nil || got != ^uint64(0) {
		t.Errorf("parseDemoWorkValue unlimited = %d, %v", got, err)
	}
	cfg := defaultDemoServerConfig()
	cfg.timingWorkMax = demoTimingWorkLimit{unlimited: true}
	mux := newDemoServerMux(demoServerEffectiveConfig{settings: cfg})
	for _, path := range []string{"/sleep?duration=0", "/work?iterations=3"} {
		if response := serveDemoRequest(mux, http.MethodGet, path); response.Code != http.StatusNoContent {
			t.Errorf("GET %s = %d, %q; want 204",
				path, response.Code, response.Body.String())
		}
	}
}

func TestDemoFixedWork(t *testing.T) {
	t.Parallel()
	for iterations, want := range map[uint64]uint64{0: 0, 1: 0, 2: 3, 3: 15} {
		if got := demoFixedWork(iterations); got != want {
			t.Errorf("demoFixedWork(%d) = %d, want %d", iterations, got, want)
		}
	}
}

func TestHalfDurationRoundUp(t *testing.T) {
	t.Parallel()
	for input, want := range map[time.Duration]time.Duration{
		0: 0, 1: 1, 2: 1, 3: 2, time.Duration(1<<63 - 1): 1 << 62,
	} {
		if got := halfDurationRoundUp(input); got != want {
			t.Errorf("halfDurationRoundUp(%s) = %s, want %s", input, got, want)
		}
	}
}

func TestDemoNetworkListenerWrapperRejectsNegativeDurations(t *testing.T) {
	t.Parallel()
	for _, config := range []demoNetworkConfig{
		{rtt: -time.Nanosecond},
		{jitter: -time.Nanosecond},
	} {
		if _, err := demoNetworkListenerWrapper(config); err == nil {
			t.Errorf("demoNetworkListenerWrapper(%+v) succeeded", config)
		}
	}
}

func TestDemoServerProtocolsAndStops(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		server, err := newDemoServer(defaultDemoServerConfig(), io.Discard)
		if server != nil || !errors.Is(err, errDemoServerUnsupported) {
			t.Fatalf("newDemoServer = %v, %v; want unsupported error", server, err)
		}
		return
	}
	cfg := defaultDemoServerConfig()
	cfg.port = 0
	server, err := newDemoServer(cfg, io.Discard)
	if err != nil {
		t.Fatalf("newDemoServer: %v", err)
	}
	t.Cleanup(func() { _ = server.http.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx) }()

	for _, test := range []struct {
		name  string
		offer []string
		want  string
	}{
		{
			name:  "HTTP/2 preferred",
			offer: []string{"http/1.1", http2.NextProtoTLS},
			want:  http2.NextProtoTLS,
		},
		{name: "HTTP/1.1 explicit", offer: []string{"http/1.1"}, want: "http/1.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientTLS := server.endpoint.ClientConfig()
			clientTLS.NextProtos = test.offer
			dialer := tls.Dialer{Config: clientTLS}
			conn, err := dialer.DialContext(
				t.Context(), "tcp", server.endpoint.Addr)
			if err != nil {
				t.Fatalf("TLS dial: %v", err)
			}
			defer func() { _ = conn.Close() }()
			tlsConn, ok := conn.(*tls.Conn)
			if !ok {
				t.Fatalf("connection type = %T, want *tls.Conn", conn)
			}
			if got := tlsConn.ConnectionState().NegotiatedProtocol; got != test.want {
				t.Errorf("negotiated protocol = %q, want %q", got, test.want)
			}
		})
	}

	transport := &http2.Transport{TLSClientConfig: server.endpoint.ClientConfig()}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, "https://"+server.endpoint.Addr+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Errorf("response protocol = %s, want HTTP/2", response.Proto)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("demo server did not stop after cancellation")
	}
}

func TestNewDemoServerReportsBindFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("demo-server is unsupported on Windows")
	}
	listener, err := (&net.ListenConfig{}).Listen(
		t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer func() { _ = listener.Close() }()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi port: %v", err)
	}
	cfg := defaultDemoServerConfig()
	cfg.port = port
	if server, err := newDemoServer(cfg, io.Discard); err == nil {
		_ = server.http.Close()
		t.Fatal("newDemoServer succeeded on an occupied address")
	}
}

func TestNewDemoServerClassifiesHTTPDiagnostics(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("demo-server is unsupported on Windows")
	}
	cfg := defaultDemoServerConfig()
	cfg.port = 0
	var output bytes.Buffer
	server, err := newDemoServer(cfg, &output)
	if err != nil {
		t.Fatalf("newDemoServer: %v", err)
	}
	defer func() { _ = server.http.Close() }()
	defer func() { _ = server.endpoint.Listener.Close() }()

	server.http.ErrorLog.Print("serving error")
	want := diagnosticPrefix(diagnosticError) + "demo-server: serving error\n"
	if got := output.String(); got != want {
		t.Errorf("HTTP diagnostic = %q, want %q", got, want)
	}
}

func TestAnnounceDemoServer(t *testing.T) {
	t.Parallel()
	cfg := demoServerEffectiveConfig{
		settings:            defaultDemoServerConfig(),
		workMaxIterations:   2_000_000_000,
		quotaWorkIterations: 50_000,
	}
	var output bytes.Buffer
	if err := announceDemoServer(
		&output, "127.0.0.1:8443", cfg,
	); err != nil {
		t.Fatalf("announceDemoServer: %v", err)
	}
	flatOutput := strings.Join(strings.Fields(output.String()), " ")
	for _, text := range []string{
		"tturl: warning:", "not intended for untrusted networks",
		"cross-site browser requests", "https://127.0.0.1:8443",
		"Limits:", "HTTP/2 streams per connection: 2048",
		"/sleep duration: 1s",
		"/work iterations: 2000000000 (calibrated for a 1s target)",
		"Quota-use work (POST /quotas/{token}/use): 50000 iterations (25us target)",
		"ephemeral, self-signed", "Ctrl-C",
	} {
		if !strings.Contains(flatOutput, text) {
			t.Errorf("startup output omits %q:\n%s", text, &output)
		}
	}
	for _, b := range output.Bytes() {
		if b < 0x0a || b > 0x7e || b > 0x0a && b < 0x20 {
			t.Errorf("startup output contains non-printable byte 0x%02x", b)
		}
	}
	for line := range strings.SplitSeq(output.String(), "\n") {
		if len(line) > textWidth {
			t.Errorf("startup output has a %d-column line: %q", len(line), line)
		}
	}
	cfg.settings.network = demoNetworkConfig{
		rtt: 200 * time.Millisecond, jitter: 20 * time.Millisecond,
	}
	if err := announceDemoServer(&output, "127.0.0.1:8443", cfg); err != nil {
		t.Fatalf("announceDemoServer with network: %v", err)
	}
	if !strings.Contains(output.String(),
		"Local propagation model: RTT floor 200ms, jitter +0..20ms.") {
		t.Errorf("startup output omits propagation configuration:\n%s", &output)
	}

	want := errors.New("write failed")
	if err := announceDemoServer(
		failingWriter{err: want}, "addr", cfg,
	); !errors.Is(err, want) {
		t.Errorf("announceDemoServer write error = %v, want %v", err, want)
	}
}

func TestRenderDemoServerStartup(t *testing.T) {
	t.Parallel()
	cfg := demoServerEffectiveConfig{
		settings:            defaultDemoServerConfig(),
		workMaxIterations:   2_000_000_000,
		quotaWorkIterations: 50_000,
	}
	want := "Listening on https://127.0.0.1:8443\n" +
		"Limits:\n" +
		"  HTTP/2 streams per connection: 2048\n" +
		"  /sleep duration: 1s\n" +
		"  /work iterations: 2000000000 (calibrated for a 1s target)\n" +
		"Quota-use work (POST /quotas/{token}/use): " +
		"50000 iterations (25us target)\n" +
		"TLS certificate: ephemeral, self-signed, and held only in memory.\n" +
		"Press Ctrl-C to stop.\n"
	if got := renderDemoServerStartup("127.0.0.1:8443", cfg); got != want {
		t.Errorf("startup output differs:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderDemoServerStartupUnlimitedWork(t *testing.T) {
	t.Parallel()
	cfg := demoServerEffectiveConfig{settings: defaultDemoServerConfig()}
	cfg.settings.timingWorkMax = demoTimingWorkLimit{unlimited: true}
	cfg.settings.quotaWorkTarget = 0
	got := renderDemoServerStartup("[::1]:1234", cfg)
	for _, text := range []string{
		"Listening on https://[::1]:1234",
		"  /sleep duration: unlimited",
		"  /work iterations: unlimited",
		"Quota-use work (POST /quotas/{token}/use): disabled",
	} {
		if !strings.Contains(got, text) {
			t.Errorf("startup output omits %q:\n%s", text, got)
		}
	}
}

func TestRenderDemoServerStartupDisabledTimingWork(t *testing.T) {
	t.Parallel()
	cfg := demoServerEffectiveConfig{
		settings: defaultDemoServerConfig(), quotaWorkIterations: 18_432,
	}
	cfg.settings.timingWorkMax = demoTimingWorkLimit{}
	got := renderDemoServerStartup("127.0.0.1:8443", cfg)
	want := "Listening on https://127.0.0.1:8443\n" +
		"Limits:\n" +
		"  HTTP/2 streams per connection: 2048\n" +
		"  /sleep duration: route disabled\n" +
		"  /work iterations: route disabled\n" +
		"Quota-use work (POST /quotas/{token}/use): " +
		"18432 iterations (25us target)\n" +
		"TLS certificate: ephemeral, self-signed, and held only in memory.\n" +
		"Press Ctrl-C to stop.\n"
	if got != want {
		t.Errorf("disabled startup output differs:\ngot:\n%s\nwant:\n%s",
			got, want)
	}
}

func TestFormatDemoDurationUsesASCII(t *testing.T) {
	t.Parallel()
	if got := formatDemoDuration(25 * time.Microsecond); got != "25us" {
		t.Errorf("formatted microseconds = %q, want 25us", got)
	}
}

func TestRunCLIDemoServerHelp(t *testing.T) {
	for _, option := range []string{"-h", "--help"} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), []string{"demo-server", option},
			commandOutput{stdout: &stdout, stderr: &stderr})
		if err != nil || code != 0 {
			t.Errorf("demo-server %s = code %d, error %v", option, code, err)
		}
		if got, want := stdout.String(),
			renderDemoServerUsage(commandByID(commandDemoServer)); got != want {
			t.Errorf("demo-server %s output differs:\n%s", option, &stdout)
		}
		if stderr.Len() != 0 {
			t.Errorf("demo-server %s stderr = %q", option, &stderr)
		}
	}

	want := errors.New("write failed")
	code, err := runCLI(t.Context(), []string{"demo-server", "-h"},
		commandOutput{stdout: failingWriter{err: want}, stderr: io.Discard})
	if code != 1 || !errors.Is(err, want) {
		t.Errorf("demo-server help write = code %d, error %v", code, err)
	}
}

func serveDemoRequest(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	return serveDemoBodyRequest(handler, method, target, "", "")
}

func assertDemoServerTimestamps(
	t *testing.T,
	response *httptest.ResponseRecorder,
	suffixes ...string,
) {
	t.Helper()
	want := demoServerTestTime.UnixNano()
	for _, suffix := range suffixes {
		name := "X-Server-Time-" + suffix
		value := response.Header().Get(name)
		timestamp, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Errorf("%s = %q, want decimal nanoseconds", name, value)
			continue
		}
		if timestamp != want {
			t.Errorf("%s = %d, want injected time %d",
				name, timestamp, want)
		}
	}
}

func assertNoDemoServerTimestamps(
	t *testing.T,
	response *httptest.ResponseRecorder,
) {
	t.Helper()
	for name := range response.Header() {
		if strings.HasPrefix(name, "X-Server-Time-") {
			t.Errorf("unexpected timing header %q", name)
		}
	}
}

func serveDemoFormRequest(
	handler http.Handler,
	target string,
	body string,
) *httptest.ResponseRecorder {
	return serveDemoBodyRequest(
		handler, http.MethodPost, target, body,
		"application/x-www-form-urlencoded")
}

func serveDemoBodyRequest(
	handler http.Handler,
	method string,
	target string,
	body string,
	contentType string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(
		context.Background(), method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
