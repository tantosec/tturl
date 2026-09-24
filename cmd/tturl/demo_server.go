package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
)

const (
	defaultDemoServerPort                 = 8443
	defaultDemoServerBindHost             = "127.0.0.1"
	defaultDemoServerTimingWorkMax        = time.Second
	defaultDemoServerConcurrentMax uint32 = 2048
	demoServerReadHeaderTimeout           = 10 * time.Second
	demoServerShutdownGrace               = 2 * time.Second
	// This bounds per-request scratch space, not the total body size.
	demoServerHashReadBufferSize = 1 << 10
)

var errDemoServerUnsupported = errors.New(
	"demo-server is unsupported on Windows because Go's clock does not " +
		"provide the resolution required for its timing routes and " +
		"diagnostic timestamps",
)

// demoServerConfig contains validated operator settings. Derived iteration
// counts belong to demoServerEffectiveConfig after startup calibration.
type demoServerConfig struct {
	port            int
	bindHost        string
	timingWorkMax   demoTimingWorkLimit
	concurrentMax   uint32
	quotaWorkTarget time.Duration
	network         demoNetworkConfig
}

// demoTimingWorkLimit distinguishes a zero-duration disabled route set from an
// explicitly unlimited one. A positive maximum is a calibrated finite limit.
type demoTimingWorkLimit struct {
	maximum   time.Duration
	unlimited bool
}

func (l demoTimingWorkLimit) disabled() bool {
	return !l.unlimited && l.maximum == 0
}

func (l demoTimingWorkLimit) limited() bool {
	return !l.unlimited && l.maximum > 0
}

func (l demoTimingWorkLimit) validate() error {
	if l.maximum < 0 {
		return fmt.Errorf("'--timing-work-max' must be non-negative")
	}
	if l.unlimited && l.maximum != 0 {
		return fmt.Errorf("'--timing-work-max' configuration is invalid")
	}
	return nil
}

// demoServerEffectiveConfig is the single configuration installed in handlers
// and reported at startup.
type demoServerEffectiveConfig struct {
	settings            demoServerConfig
	workMaxIterations   uint64
	quotaWorkIterations uint64
}

type demoNetworkConfig struct {
	rtt    time.Duration
	jitter time.Duration
}

func defaultDemoServerConfig() demoServerConfig {
	return demoServerConfig{
		port:     defaultDemoServerPort,
		bindHost: defaultDemoServerBindHost,
		timingWorkMax: demoTimingWorkLimit{
			maximum: defaultDemoServerTimingWorkMax,
		},
		concurrentMax:   defaultDemoServerConcurrentMax,
		quotaWorkTarget: defaultDemoQuotaWork,
	}
}

func runDemoServerCommand(
	ctx context.Context,
	spec *commandSpec,
	_ []commandSpec,
	args []string,
	out commandOutput,
) error {
	out, selectionErr := commandDiagnosticOutput(spec, args, out)
	if selectionErr != nil {
		return selectionErr
	}
	usage := renderDemoServerUsage(spec)
	cfg, help, err := parseDemoServerArgs(args)
	if err != nil {
		return fmt.Errorf("%w; run '%s demo-server -h' for usage", err, toolName)
	}
	if help {
		_, err := fmt.Fprint(out.stdout, usage)
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := announceCommand(out.stderr, spec); err != nil {
		return err
	}

	server, err := newDemoServer(cfg, out.stderr)
	if err != nil {
		return err
	}
	if err := announceDemoServer(out.stderr, server.endpoint.Addr, server.config); err != nil {
		_ = server.http.Close()
		return err
	}
	return server.serve(ctx)
}

func renderDemoServerUsage(spec *commandSpec) string {
	return normaliseHelpText(fmt.Sprintf(
		toolName+" "+spec.name+" - "+spec.summary+"\n\n"+
			"Usage:\n"+
			"  "+toolName+" "+spec.name+" [options]\n\n"+
			"Options:\n"+
			"  -p, --port PORT                  listen on PORT (default %d; 0 = free)\n"+
			"  -b, --bind-host HOST             bind to HOST (default %s)\n"+
			"      --timing-work-max DURATION|unlimited\n"+
			"                                     limit /sleep and /work timing work\n"+
			"                                     (default %s; 0 disables routes)\n"+
			"      --max-concurrent-streams N   limit HTTP/2 streams per connection\n"+
			"                                     (default %d)\n"+
			"      --quota-work DURATION         target work between quota check and update\n"+
			"                                     (default %s; 0 = disabled)\n"+
			"      --rtt DURATION                add a nonnegative round-trip-time floor\n"+
			"                                     (default 0)\n"+
			"      --jitter DURATION             add up to nonnegative DURATION RTT variation\n"+
			"                                     (default 0)\n"+
			"  -s, --silent       "+silentDescription+"\n"+
			"  -h, --help                       show this help and exit\n\n"+
			"Guide:\n"+
			"  "+toolName+" help demo-server\n",
		defaultDemoServerPort,
		defaultDemoServerBindHost,
		formatDemoDuration(defaultDemoServerTimingWorkMax),
		defaultDemoServerConcurrentMax,
		formatDemoDuration(defaultDemoQuotaWork),
	))
}

func parseDemoServerArgs(args []string) (demoServerConfig, bool, error) {
	cfg := defaultDemoServerConfig()
	var help bool
	fs, timingWorkMax, concurrentMax := newDemoServerFlagSet(&cfg, &help)
	if err := fs.Parse(args); err != nil {
		return demoServerConfig{}, false, fmt.Errorf("parse options: %w", err)
	}
	maximum, unlimited, err := parseDurationOrUnlimited(*timingWorkMax)
	if err != nil {
		return demoServerConfig{}, false, fmt.Errorf(
			"'--timing-work-max' %w", err)
	}
	cfg.timingWorkMax = demoTimingWorkLimit{
		maximum: maximum, unlimited: unlimited,
	}
	if help {
		return cfg, true, nil
	}
	if fs.NArg() != 0 {
		return demoServerConfig{}, false, fmt.Errorf(
			"unexpected argument %q", fs.Arg(0))
	}
	if err := validateDemoServerConfig(&cfg, *concurrentMax); err != nil {
		return demoServerConfig{}, false, err
	}
	return cfg, false, nil
}

func newDemoServerFlagSet(
	cfg *demoServerConfig,
	help *bool,
) (*pflag.FlagSet, *string, *uint64) {
	fs := pflag.NewFlagSet(toolName+" demo-server", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVarP(&cfg.port, "port", "p", cfg.port, "listen on PORT")
	fs.StringVarP(&cfg.bindHost, "bind-host", "b", cfg.bindHost,
		"bind to HOST")
	timingWorkMax := new(string)
	*timingWorkMax = formatDemoTimingWorkFlagValue(cfg.timingWorkMax)
	fs.StringVar(timingWorkMax, "timing-work-max", *timingWorkMax,
		"limit /sleep and /work timing work")
	fs.Lookup("timing-work-max").Annotations = map[string][]string{
		completionValuesFlagAnnotation: {unlimitedFlagValue},
	}
	concurrentMax := new(uint64)
	*concurrentMax = uint64(cfg.concurrentMax)
	fs.Uint64Var(concurrentMax, "max-concurrent-streams",
		*concurrentMax, "limit HTTP/2 streams per connection")
	fs.DurationVar(&cfg.quotaWorkTarget, "quota-work", cfg.quotaWorkTarget,
		"target work between quota check and update")
	fs.DurationVar(&cfg.network.rtt, "rtt", cfg.network.rtt,
		"add a round-trip-time floor")
	fs.DurationVar(&cfg.network.jitter, "jitter", cfg.network.jitter,
		"add up to DURATION RTT variation")
	fs.BoolP("silent", "s", false, silentDescription)
	fs.BoolVarP(help, "help", "h", false, "show this help and exit")
	return fs, timingWorkMax, concurrentMax
}

func validateDemoServerConfig(cfg *demoServerConfig, concurrentMax uint64) error {
	if cfg.port < 0 || cfg.port > math.MaxUint16 {
		return fmt.Errorf(
			"'--port' must be between 0 and %d", math.MaxUint16)
	}
	if err := cfg.timingWorkMax.validate(); err != nil {
		return err
	}
	if cfg.quotaWorkTarget < 0 {
		return fmt.Errorf("'--quota-work' must be non-negative")
	}
	if cfg.network.rtt < 0 {
		return fmt.Errorf("'--rtt' must be non-negative")
	}
	if cfg.network.jitter < 0 {
		return fmt.Errorf("'--jitter' must be non-negative")
	}
	if concurrentMax == 0 || concurrentMax > math.MaxUint32 {
		return fmt.Errorf(
			"'--max-concurrent-streams' must be between 1 and %d", uint64(math.MaxUint32))
	}
	cfg.concurrentMax = uint32(concurrentMax)
	return nil
}

type demoServer struct {
	endpoint *h2tls.Endpoint
	http     *http.Server
	config   demoServerEffectiveConfig
}

func newDemoServer(cfg demoServerConfig, errorOutput io.Writer) (*demoServer, error) {
	if runtime.GOOS == "windows" {
		return nil, errDemoServerUnsupported
	}
	return newDemoServerWithWorkMeasure(cfg, errorOutput, measureDemoFixedWork)
}

func newDemoServerWithWorkMeasure(
	cfg demoServerConfig,
	errorOutput io.Writer,
	measure func(uint64) time.Duration,
) (*demoServer, error) {
	effective, err := resolveDemoServerConfig(cfg, measure)
	if err != nil {
		return nil, err
	}
	networkWrapper, err := demoNetworkListenerWrapper(effective.settings.network)
	if err != nil {
		return nil, err
	}
	cert, err := h2tls.SelfSignedServerCert(h2tls.WithCommonName(toolName))
	if err != nil {
		return nil, fmt.Errorf("generate certificate: %w", err)
	}
	addr := net.JoinHostPort(
		effective.settings.bindHost, strconv.Itoa(effective.settings.port))
	endpoint, err := h2tls.Listen(
		cert,
		h2tls.WithAddr(addr),
		h2tls.WithProtocols(http2.NextProtoTLS, "http/1.1"),
		h2tls.WithListenerWrapper(networkWrapper),
	)
	if err != nil {
		return nil, fmt.Errorf("listen with '--bind-host' %q and '--port' %d: %w",
			curlblocks.DisplayText(effective.settings.bindHost), effective.settings.port, err)
	}

	srv := &http.Server{
		Handler:           newDemoServerMux(effective),
		ReadHeaderTimeout: demoServerReadHeaderTimeout,
		ErrorLog: log.New(diagnosticLogWriter{
			w: errorOutput, nature: diagnosticError, context: "demo-server: ",
		}, "", 0),
	}
	if err := http2.ConfigureServer(srv, &http2.Server{
		MaxConcurrentStreams: effective.settings.concurrentMax,
	}); err != nil {
		_ = endpoint.Listener.Close()
		return nil, fmt.Errorf("configure HTTP/2 server: %w", err)
	}
	return &demoServer{endpoint: endpoint, http: srv, config: effective}, nil
}

func demoNetworkListenerWrapper(
	config demoNetworkConfig,
) (func(net.Listener) net.Listener, error) {
	if config.rtt < 0 {
		return nil, fmt.Errorf(
			"configure local propagation model: rtt must be non-negative")
	}
	if config.jitter < 0 {
		return nil, fmt.Errorf(
			"configure local propagation model: jitter must be non-negative")
	}
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay:     halfDurationRoundUp(config.rtt),
		Variation: config.jitter / 2,
	})
	if err != nil {
		return nil, fmt.Errorf("configure local propagation model: %w", err)
	}
	return wrapper, nil
}

func halfDurationRoundUp(duration time.Duration) time.Duration {
	return duration/2 + duration%2
}

func (s *demoServer) serve(ctx context.Context) error {
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- s.http.Serve(s.endpoint.Listener)
	}()

	select {
	case err := <-serveDone:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), demoServerShutdownGrace)
		defer cancel()
		shutdownErr := s.http.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = s.http.Close()
		}
		serveErr := <-serveDone
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve during shutdown: %w", serveErr)
		}
		if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
			return fmt.Errorf("shut down: %w", shutdownErr)
		}
		return nil
	}
}

func announceDemoServer(
	w io.Writer,
	addr string,
	cfg demoServerEffectiveConfig,
) error {
	if err := writeDiagnostic(w, diagnosticWarning,
		"demo-server is not intended for untrusted networks. Its routes "+
			"deliberately consume CPU. Public clients or cross-site browser "+
			"requests can consume substantial CPU."); err != nil {
		return err
	}
	_, err := io.WriteString(w, renderDemoServerStartup(addr, cfg))
	return err
}

func renderDemoServerStartup(addr string, cfg demoServerEffectiveConfig) string {
	var b strings.Builder
	settings := cfg.settings
	fmt.Fprintf(&b,
		"Listening on https://%s\n"+
			"Limits:\n"+
			"  HTTP/2 streams per connection: %d\n",
		addr, settings.concurrentMax)
	switch {
	case settings.timingWorkMax.disabled():
		b.WriteString("  /sleep duration: route disabled\n")
		b.WriteString("  /work iterations: route disabled\n")
	case settings.timingWorkMax.unlimited:
		b.WriteString("  /sleep duration: unlimited\n")
		b.WriteString("  /work iterations: unlimited\n")
	default:
		maximum := formatDemoDuration(settings.timingWorkMax.maximum)
		fmt.Fprintf(&b, "  /sleep duration: %s\n", maximum)
		writeWrappedASCII(&b, "  /work iterations: ", fmt.Sprintf(
			"%d (calibrated for a %s target)",
			cfg.workMaxIterations, maximum))
	}
	if settings.quotaWorkTarget == 0 {
		b.WriteString(
			"Quota-use work (POST /quotas/{token}/use): disabled\n")
	} else {
		writeWrappedASCII(&b,
			"Quota-use work (POST /quotas/{token}/use): ", fmt.Sprintf(
				"%d iterations (%s target)", cfg.quotaWorkIterations,
				formatDemoDuration(settings.quotaWorkTarget)))
	}
	if settings.network.rtt != 0 || settings.network.jitter != 0 {
		writeWrappedASCII(&b, "Local propagation model: ", fmt.Sprintf(
			"RTT floor %s, jitter +0..%s.",
			formatDemoDuration(settings.network.rtt),
			formatDemoDuration(settings.network.jitter)))
	}
	b.WriteString(
		"TLS certificate: ephemeral, self-signed, and held only in memory.\n" +
			"Press Ctrl-C to stop.\n")
	return b.String()
}

func formatDemoDuration(duration time.Duration) string {
	return formatDurationASCII(duration)
}

func formatDemoTimingWorkFlagValue(limit demoTimingWorkLimit) string {
	if limit.unlimited {
		return unlimitedFlagValue
	}
	return formatDemoDuration(limit.maximum)
}

var demoServerRouteIndex = normaliseHelpText(
	toolName + " demo-server - local HTTP/2 education and research target\n\n" +
		"Routes:\n" +
		"  GET /                         list these routes\n" +
		"  GET /sleep?duration=D         spin for duration D, then return 204\n" +
		"  POST /sleep                   read duration=D form data, then spin\n" +
		"  GET /work?iterations=N        perform N fixed-work iterations, then return 204\n" +
		"  POST /work                    read iterations=N form data, then work\n" +
		"  POST /sha256                  return the request body's SHA-256 digest\n" +
		"  GET /now                      return Unix time in decimal nanoseconds\n\n" +
		"  POST /quotas                  create a one-use quota\n" +
		"  GET /quotas/{token}           inspect a quota\n" +
		"  POST /quotas/{token}/use      attempt one quota use\n\n" +
		"Durations use Go syntax such as 100us, 1.5ms, or 2s.\n" +
		"Run '" + toolName + " help demo-server' for safety and experiment guidance.\n")

type demoServerRoutes struct {
	timingWorkDisabled  bool
	sleepDurationMax    time.Duration
	workIterationsMax   uint64
	quotaWorkIterations uint64
	quotas              *demoQuotaStore
	clock               func() time.Time
}

func newDemoServerMux(cfg demoServerEffectiveConfig) *http.ServeMux {
	return newDemoServerMuxAt(cfg, time.Now)
}

func newDemoServerMuxAt(
	cfg demoServerEffectiveConfig,
	now func() time.Time,
) *http.ServeMux {
	routes := &demoServerRoutes{
		timingWorkDisabled:  cfg.settings.timingWorkMax.disabled(),
		sleepDurationMax:    cfg.settings.timingWorkMax.maximum,
		workIterationsMax:   cfg.workMaxIterations,
		quotaWorkIterations: cfg.quotaWorkIterations,
		quotas:              newDemoQuotaStore(demoQuotaStoreSize, rand.Reader),
		clock:               now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", routes.index)
	mux.HandleFunc("GET /sleep", routes.sleep)
	mux.HandleFunc("POST /sleep", routes.sleepForm)
	mux.HandleFunc("GET /work", routes.work)
	mux.HandleFunc("POST /work", routes.workForm)
	mux.HandleFunc("POST /sha256", routes.sha256)
	mux.HandleFunc("GET /now", routes.now)
	mux.HandleFunc("POST /quotas", routes.createQuota)
	mux.HandleFunc("GET /quotas/{token}", routes.quotaStatus)
	mux.HandleFunc("POST /quotas/{token}/use", routes.useQuota)
	return mux
}

func (s *demoServerRoutes) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, demoServerRouteIndex)
}

func (s *demoServerRoutes) sleep(w http.ResponseWriter, r *http.Request) {
	if s.rejectDisabledTimingWork(w) {
		return
	}
	handlerStart := s.clock().UnixNano()
	value, err := demoQueryValue(r.URL.RawQuery, "duration")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	duration, err := parseDemoSleepValue(value, s.sleepDurationMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sleepStart := s.clock().UnixNano()
	demoSpinWait(duration)
	sleepEnd := s.clock().UnixNano()
	header := w.Header()
	header.Set("X-Server-Time-Handler-Start", strconv.FormatInt(handlerStart, 10))
	header.Set("X-Server-Time-Sleep-Start", strconv.FormatInt(sleepStart, 10))
	header.Set("X-Server-Time-Sleep-End", strconv.FormatInt(sleepEnd, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *demoServerRoutes) sleepForm(w http.ResponseWriter, r *http.Request) {
	if s.rejectDisabledTimingWork(w) {
		return
	}
	handlerStart := s.clock().UnixNano()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid URL-encoded form body", http.StatusBadRequest)
		return
	}
	values := r.PostForm["duration"]
	if len(values) != 1 || values[0] == "" {
		http.Error(w, "expected exactly one duration form field",
			http.StatusBadRequest)
		return
	}
	duration, err := parseDemoSleepValue(values[0], s.sleepDurationMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sleepStart := s.clock().UnixNano()
	demoSpinWait(duration)
	sleepEnd := s.clock().UnixNano()
	header := w.Header()
	header.Set("X-Server-Time-Handler-Start", strconv.FormatInt(handlerStart, 10))
	header.Set("X-Server-Time-Sleep-Start", strconv.FormatInt(sleepStart, 10))
	header.Set("X-Server-Time-Sleep-End", strconv.FormatInt(sleepEnd, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *demoServerRoutes) work(w http.ResponseWriter, r *http.Request) {
	if s.rejectDisabledTimingWork(w) {
		return
	}
	handlerStart := s.clock().UnixNano()
	value, err := demoQueryValue(r.URL.RawQuery, "iterations")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	iterations, err := parseDemoWorkValue(value, s.workIterationsMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workStart := s.clock().UnixNano()
	demoFixedWork(iterations) //nolint:staticcheck // CPU-burn result is deliberately discarded
	workEnd := s.clock().UnixNano()
	header := w.Header()
	header.Set("X-Server-Time-Handler-Start", strconv.FormatInt(handlerStart, 10))
	header.Set("X-Server-Time-Work-Start", strconv.FormatInt(workStart, 10))
	header.Set("X-Server-Time-Work-End", strconv.FormatInt(workEnd, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *demoServerRoutes) workForm(w http.ResponseWriter, r *http.Request) {
	if s.rejectDisabledTimingWork(w) {
		return
	}
	handlerStart := s.clock().UnixNano()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid URL-encoded form body", http.StatusBadRequest)
		return
	}
	values := r.PostForm["iterations"]
	if len(values) != 1 || values[0] == "" {
		http.Error(w, "expected exactly one iterations form field",
			http.StatusBadRequest)
		return
	}
	iterations, err := parseDemoWorkValue(values[0], s.workIterationsMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	workStart := s.clock().UnixNano()
	demoFixedWork(iterations) //nolint:staticcheck // CPU-burn result is deliberately discarded
	workEnd := s.clock().UnixNano()
	header := w.Header()
	header.Set("X-Server-Time-Handler-Start", strconv.FormatInt(handlerStart, 10))
	header.Set("X-Server-Time-Work-Start", strconv.FormatInt(workStart, 10))
	header.Set("X-Server-Time-Work-End", strconv.FormatInt(workEnd, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *demoServerRoutes) sha256(w http.ResponseWriter, r *http.Request) {
	handlerStart := s.clock().UnixNano()
	hash := sha256.New()
	var buffer [demoServerHashReadBufferSize]byte
	hashStart := s.clock().UnixNano()
	if _, err := io.CopyBuffer(hash, r.Body, buffer[:]); err != nil {
		http.Error(w, "could not read complete request body: "+err.Error(),
			http.StatusBadRequest)
		return
	}
	hashEnd := s.clock().UnixNano()
	var body [sha256.Size * 2]byte
	hex.Encode(body[:], hash.Sum(nil))
	header := w.Header()
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("X-Server-Time-Handler-Start", strconv.FormatInt(handlerStart, 10))
	header.Set("X-Server-Time-Hash-Start", strconv.FormatInt(hashStart, 10))
	header.Set("X-Server-Time-Hash-End", strconv.FormatInt(hashEnd, 10))
	_, _ = w.Write(body[:])
}

func (s *demoServerRoutes) now(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var storage [20]byte
	body := strconv.AppendInt(storage[:0], s.clock().UnixNano(), 10)
	_, _ = w.Write(body)
}

func (s *demoServerRoutes) rejectDisabledTimingWork(w http.ResponseWriter) bool {
	if !s.timingWorkDisabled {
		return false
	}
	http.Error(w, "timing work is disabled by server configuration",
		http.StatusForbidden)
	return true
}

func parseDemoSleepValue(value string, maximum time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, fmt.Errorf("duration must be a non-negative Go duration")
	}
	if maximum > 0 && duration > maximum {
		return 0, fmt.Errorf("duration exceeds the server limit of %s", maximum)
	}
	return duration, nil
}

func parseDemoWorkValue(value string, maximum uint64) (uint64, error) {
	iterations, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("iterations must be an unsigned decimal integer")
	}
	if maximum > 0 && iterations > maximum {
		return 0, fmt.Errorf("iterations exceed the server limit of %d", maximum)
	}
	return iterations, nil
}

func demoQueryValue(rawQuery, name string) (string, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid URL query: %w", err)
	}
	matching := values[name]
	if len(matching) != 1 || matching[0] == "" {
		return "", fmt.Errorf("expected exactly one %s query parameter", name)
	}
	return matching[0], nil
}

func demoSpinWait(duration time.Duration) {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
	}
}

// demoFixedWork performs fixed arithmetic using only loop-local state, with no
// shared state or cancellation checks. Returning the accumulator and keeping
// the function out of line prevents compiler elimination.
//
//go:noinline
func demoFixedWork(iterations uint64) uint64 {
	var accumulator uint64
	for i := range iterations {
		accumulator += i
		accumulator ^= accumulator << 1
	}
	return accumulator
}
