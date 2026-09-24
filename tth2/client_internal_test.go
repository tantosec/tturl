package tth2

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestReleaseDelayOptionAppliesToBothEntryPoints checks the option plumbing
// independently of a wall clock: the same explicit duration reaches both send
// and trial configuration.
func TestReleaseDelayOptionAppliesToBothEntryPoints(t *testing.T) {
	t.Parallel()
	const delay = 37 * time.Millisecond
	option := WithReleaseDelay(delay)

	send := defaultSendConfig()
	option.applySend(&send)
	if send.releaseDelay != delay {
		t.Errorf("send release delay = %v, want %v", send.releaseDelay, delay)
	}

	trials := defaultTrialsConfig()
	option.applyTrials(&trials)
	if trials.send.releaseDelay != delay {
		t.Errorf("trials release delay = %v, want %v",
			trials.send.releaseDelay, delay)
	}
}

func TestMinBatchIntervalOption(t *testing.T) {
	t.Parallel()
	const interval = 37 * time.Millisecond
	cfg := defaultTrialsConfig()
	WithMinBatchInterval(interval).applyTrials(&cfg)
	if cfg.minBatchInterval != interval {
		t.Errorf("minimum batch interval = %v, want %v",
			cfg.minBatchInterval, interval)
	}
}

// TestBodyBytesWithheldOptionAppliesToBothEntryPoints checks that the body
// release strategy reaches both direct sends and every send in a trial run.
func TestBodyBytesWithheldOptionAppliesToBothEntryPoints(t *testing.T) {
	t.Parallel()
	const withheld = 37
	option := WithBodyBytesWithheld(withheld)

	send := defaultSendConfig()
	option.applySend(&send)
	if send.bodyBytesWithheld != withheld {
		t.Errorf("send body bytes withheld = %d, want %d",
			send.bodyBytesWithheld, withheld)
	}

	trials := defaultTrialsConfig()
	option.applyTrials(&trials)
	if trials.send.bodyBytesWithheld != withheld {
		t.Errorf("trials body bytes withheld = %d, want %d",
			trials.send.bodyBytesWithheld, withheld)
	}
}

func TestResponseCaptureOptionsApplyToSendAndStreamConfig(t *testing.T) {
	t.Parallel()
	body := WithResponseBodyCaptureBytes(37)
	headers := WithResponseHeaderCapture()

	send := defaultSendConfig()
	body.applySend(&send)
	headers.applySend(&send)
	if send.responseBodyCaptureBytes != 37 || !send.responseHeaderCapture {
		t.Errorf("send capture = %d/%t, want 37/true",
			send.responseBodyCaptureBytes, send.responseHeaderCapture)
	}

	trials := defaultTrialsConfig()
	body.applyTrials(&trials)
	headers.applyTrials(&trials)
	if trials.send.responseBodyCaptureBytes != 37 ||
		!trials.send.responseHeaderCapture {
		t.Errorf("trials capture = %d/%t, want 37/true",
			trials.send.responseBodyCaptureBytes,
			trials.send.responseHeaderCapture)
	}
}

func TestResponseLimitOptionsApplyToEveryBatchEntryPoint(t *testing.T) {
	t.Parallel()
	body := WithMaxResponseBodyBytes(37)
	timeout := WithBatchTimeout(41 * time.Millisecond)

	send := defaultSendConfig()
	body.applySend(&send)
	timeout.applySend(&send)
	if send.maxResponseBodyBytes != 37 ||
		send.batchTimeout != 41*time.Millisecond {
		t.Errorf("send limits = %d/%v, want 37/41ms",
			send.maxResponseBodyBytes, send.batchTimeout)
	}

	trials := defaultTrialsConfig()
	body.applyTrials(&trials)
	timeout.applyTrials(&trials)
	if trials.send.maxResponseBodyBytes != 37 ||
		trials.send.batchTimeout != 41*time.Millisecond {
		t.Errorf("trial limits = %d/%v, want 37/41ms",
			trials.send.maxResponseBodyBytes, trials.send.batchTimeout)
	}

	var _ SendOption = body
	var _ RunTrialsOption = body
	var _ SendOption = timeout
	var _ RunTrialsOption = timeout
}

func TestOptionApplicability(t *testing.T) {
	t.Parallel()
	common := []RunTrialsOption{
		WithWarmup(0),
		WithWarmupRequests(new(http.Request)),
		WithMaxConns(0),
		WithMaxRequestsPerSecond(0),
		WithMinBatchInterval(0),
		WithArrangementPolicy(ArrangeNone),
		WithProgress(time.Nanosecond, func(TrialProgress) {}),
		WithReleaseDelay(0),
		WithBodyBytesWithheld(0),
		WithPeerStreamLimitIgnored(),
		WithPadding(),
		WithMaxResponseBodyBytes(0),
		WithBatchTimeout(0),
	}
	for _, option := range common {
		var _ TrialsOption = option
	}

	streamOnly := []TrialsOption{
		WithMaxTrials(1),
		WithResponseBodyCaptureBytes(0),
		WithResponseHeaderCapture(),
	}
	for _, option := range streamOnly {
		if _, ok := option.(RunTrialsOption); ok {
			t.Errorf("%T unexpectedly implements RunTrialsOption", option)
		}
	}

	var _ SendOption = WithResponseBodyCaptureBytes(0)
	var _ SendOption = WithResponseHeaderCapture()
}

// TestClientTransportSelection checks both branches of the zero-value contract:
// an explicit transport is preserved and nil resolves to DefaultTransport.
func TestClientTransportSelection(t *testing.T) {
	t.Parallel()
	custom := new(Transport)
	if got := (&Client{Transport: custom}).transport(); got != custom {
		t.Errorf("explicit transport = %p, want %p", got, custom)
	}
	if got := new(Client).transport(); got != DefaultTransport {
		t.Errorf("zero-value transport = %p, want DefaultTransport %p",
			got, DefaultTransport)
	}
}

// TestWaitReleaseDelay checks the delay helper's three control-flow contracts:
// non-positive durations do nothing, cancellation interrupts a pending delay,
// and a positive duration can expire normally.
func TestWaitReleaseDelay(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitReleaseDelay(cancelled, 0); err != nil {
		t.Errorf("zero delay with cancelled context = %v, want nil", err)
	}
	if err := waitReleaseDelay(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled delay = %v, want context.Canceled", err)
	}
	if err := waitReleaseDelay(t.Context(), time.Nanosecond); err != nil {
		t.Errorf("expired delay = %v, want nil", err)
	}
}
