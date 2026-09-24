package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestAnalyseCommandTextAndJSON(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	base := []string{
		"--insecure", "--cycles", "6",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}

	t.Run("text", func(t *testing.T) {
		out, stdout, stderr := captureOutput(t)
		if err := runAnalyse(t.Context(), base, out); err != nil {
			t.Fatalf("runAnalyse: %v", err)
		}
		if got := strings.Count(stdout.String(), centre(
			toolName+" analyse", textWidth)); got != 1 {
			t.Errorf("report has %d analyse banners, want 1:\n%s", got, stdout)
		}
		for _, want := range []string{
			"Requests", "Experiment", "Analysis result", "Finding:",
			"Evidence:", "TLS: certificate verification disabled (--insecure).",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("text report omits %q:\n%s", want, stdout)
			}
		}
		if !strings.Contains(stderr.String(),
			"tturl: warning: 6 cycles is below the 150-cycle modelled default") {
			t.Errorf("stderr omits sensitivity warning:\n%s", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		out, stdout, stderr := captureOutput(t)
		args := append([]string{
			"--report", "json", "--connections", "2",
		}, base...)
		if err := runAnalyse(t.Context(), args, out); err != nil {
			t.Fatalf("runAnalyse JSON: %v", err)
		}
		document := decodeAnalyseJSONL(t, stdout.Bytes())
		if document.Schema != analyseSchema.logical || document.Plan.BatchWidth != 2 ||
			document.Plan.Work.PlannedCycles != 6 || document.Acquisition.Delivery.ConnectionLimit != 2 ||
			document.Plan.ModelledDefaultCycles != analyseDefaultCycles(2) ||
			!document.Plan.BelowModelledDefault ||
			document.Acquisition.ResponseLimits.BodyBytes == nil ||
			*document.Acquisition.ResponseLimits.BodyBytes != defaultResponseBodyMax ||
			document.Acquisition.ResponseLimits.BatchTimeoutNS == nil ||
			*document.Acquisition.ResponseLimits.BatchTimeoutNS !=
				defaultBatchTimeout.Nanoseconds() {
			t.Errorf("analyse JSON design = %+v, schema %q",
				document.Plan, document.Schema)
		}
		if document.Acquisition.Capture.Headers ||
			document.Acquisition.Capture.BodyBytes == nil || *document.Acquisition.Capture.BodyBytes != 0 ||
			document.Acquisition.Delivery.ConnectionCeiling != 2 ||
			document.Acquisition.Delivery.ConnectionLimit != 2 ||
			document.Acquisition.Delivery.Selection.Mode != "fixed" ||
			len(document.Evidence.Requests) != 2 ||
			len(document.Evidence.Trials) != 12 ||
			document.Evidence.Requests[0].URL == "" {
			t.Errorf("analyse JSON evidence = %+v", document.Evidence)
		}
		if len(document.Evidence.Trials) > 0 {
			firstDispatch := document.Evidence.Trials[0].Dispatch
			if firstDispatch.HeadersSentAt == nil ||
				firstDispatch.BodyFinalisedAt != nil {
				t.Errorf("bodyless analyse dispatch = %+v", firstDispatch)
			}
		}
		replayed, err := replayAnalyseJSONStream(bytes.NewReader(stdout.Bytes()))
		if err != nil {
			t.Fatalf("replay analyse JSON: %v", err)
		}
		if makeJSONAnalyseResultRecord(replayed,
			analyseJSONContext{}).Analysis.Finding.Status != document.ResultRecord.Analysis.Finding.Status ||

			replayed.Execution.RetainedTrials != 12 {
			t.Errorf("replayed analyse result = %+v", replayed)
		}
		for _, want := range []string{
			"Target:", "Total: 2 requests.",
			"tturl: warning: 6 cycles is below the 150-cycle modelled default",
			"up to 2 concurrent connections can change the target signal",
		} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("JSON stderr omits %q:\n%s", want, stderr)
			}
		}
	})
}

func TestAnalyseDefaultWorkloadHasNoPreflightWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, _, stderr := captureOutput(t)
	err := runAnalyse(ctx, []string{
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, out)
	if !errors.Is(err, errAnalyseUnavailable) {
		t.Fatalf("runAnalyse error = %v, want unavailable", err)
	}
	if strings.Contains(stderr.String(), diagnosticPrefix(diagnosticWarning)) {
		t.Errorf("modelled-default workload emitted a warning:\n%s", stderr)
	}
}

func TestAnalyseCommandResponseCapture(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("X-Capture", "present")
		_, _ = io.WriteString(w, "captured body")
	}))
	out, stdout, _ := captureOutput(t)
	err := runAnalyse(t.Context(), []string{
		"--report", "json", "--insecure", "--cycles", "6",
		"--capture-headers", "--capture-body", "unlimited",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runAnalyse: %v", err)
	}
	document := decodeAnalyseJSONL(t, stdout.Bytes())
	response := document.Evidence.Trials[0].Outcomes[0].Response
	if response == nil || response.Headers == nil || response.Body == nil ||
		!reflect.DeepEqual((*response.Headers)["x-capture"], []string{"present"}) ||
		string(response.Body.Data) != "Y2FwdHVyZWQgYm9keQ==" {
		t.Fatalf("response capture = %+v", response)
	}
	if _, err := replayAnalyseJSONStream(bytes.NewReader(stdout.Bytes())); err != nil {
		t.Fatalf("replay captured stream: %v", err)
	}
}

func TestAnalyseCommandCancellationReportsUnavailable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runAnalyse(ctx, []string{
		"--cycles", "6",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, out)
	if !errors.Is(err, errAnalyseUnavailable) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("runAnalyse error = %v, want unavailable cancellation", err)
	}
	for _, want := range []string{
		"Finding unavailable:", "Code: interrupted.",
		"Trials: 0/12 attempted; 0 rank-complete; 0 incomplete; 0 retained",
		"Completion: stopped.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("unavailable report omits %q:\n%s", want, stdout)
		}
	}
}

func TestAnalyseCLIUnavailableExitPointsToReport(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code, err := runCLI(ctx, []string{
		"analyse", "--cycles", "6",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, commandOutput{stdout: &stdout, stderr: &stderr})
	if code != 1 || !errors.Is(err, errAnalyseUnavailable) {
		t.Fatalf("runCLI = code %d, error %v; want unavailable exit 1",
			code, err)
	}
	if strings.Contains(stderr.String(), diagnosticPrefix(diagnosticError)) ||
		strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr adds a second failure after the terminal report:\n%s", &stderr)
	}
}

func TestAnalyseUnavailablePreservesOutputCloseFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	want := errors.New("close failed")
	file := &closeErrorBuffer{err: want}
	err := runAnalyse(ctx, []string{
		"--cycles", "6", "--output", "report.txt",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, commandOutput{
		stdout: io.Discard, stderr: io.Discard,
		openOutput: func(string) (io.WriteCloser, error) { return file, nil },
	})
	if !errors.Is(err, want) || errors.Is(err, errAnalyseUnavailable) {
		t.Errorf("runAnalyse error = %v, want only close failure", err)
	}
	if !strings.Contains(file.String(), "Finding unavailable:") {
		t.Errorf("output lacks unavailable report:\n%s", file.String())
	}
}

func TestNewAnalyseSeed(t *testing.T) {
	seed, err := newAnalyseSeed(bytes.NewReader(
		[]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil || seed != 0x0807060504030201 {
		t.Errorf("newAnalyseSeed = %#x, %v", seed, err)
	}
	want := errors.New("entropy failed")
	if _, err := newAnalyseSeed(failingReader{err: want}); !errors.Is(err, want) {
		t.Errorf("newAnalyseSeed error = %v, want %v", err, want)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = failingReader{}
