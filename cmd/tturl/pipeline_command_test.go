package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
)

func TestCommandsReviewLargeAutomaticFitBeforeHTTPWork(t *testing.T) {
	const oneWay = 5 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	var handled atomic.Int64
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		handled.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}), h2tls.WithListenerWrapper(wrapper))
	common := []string{
		"-k", "--connections-fit", "--batch-rate-max", "20000/s", "--repeat", "8",
		"https://" + addr + "/control",
	}
	tests := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		args []string
	}{
		{"race", runRace, append([]string{"--trials", "129", "--arrange", "none"}, common...)},
		{"measure", runMeasure, append([]string{"--trials", "129", "--arrange", "none"}, common...)},
		{"analyse", runAnalyse, append([]string{"--cycles", "129"}, common...)},
		{"detect", runDetect, append([]string{
			"--direction", "late", "--comparisons-max", "129", "--width", "8",
		}, common...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := handled.Load()
			out, stdout, _ := captureOutput(t)
			err := test.run(context.Background(), test.args, out)
			if err == nil ||
				!strings.Contains(err.Error(), "1024 streams") ||
				!strings.Contains(err.Error(), "--connections-fit-max 129") {
				t.Fatalf("run error = %v", err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout before review failure:\n%s", stdout.String())
			}
			if got := handled.Load(); got != before {
				t.Errorf("HTTP handler calls = %d, want %d", got, before)
			}
		})
	}
}

func TestCommandsNarrowLargeRawFitToFiniteWork(t *testing.T) {
	const oneWay = 5 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  2,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}), h2tls.WithListenerWrapper(wrapper))
	common := []string{
		"-k", "--report", "json", "--connections-fit", "--batch-rate-max", "100000/s",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}
	tests := []struct {
		name      string
		run       func(context.Context, []string, commandOutput) error
		args      []string
		wantLimit int
		analyse   bool
	}{
		{"race", runRace, append([]string{"--trials", "2", "--arrange", "none"}, common...), 2, false},
		{"measure", runMeasure, append([]string{"--trials", "2", "--arrange", "none"}, common...), 2, false},
		{"analyse", runAnalyse, append([]string{"--cycles", "6"}, common...), 6, true},
		{"detect", runDetect, append([]string{
			"--direction", "late", "--comparisons-max", "2",
		}, common...), 2, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, stdout, _ := captureOutput(t)
			if err := test.run(context.Background(), test.args, out); err != nil {
				t.Fatalf("run: %v", err)
			}

			var run struct {
				Acquisition struct {
					Delivery structuredDelivery `json:"delivery"`
				} `json:"acquisition"`
			}
			if err := json.NewDecoder(stdout).Decode(&run); err != nil {
				t.Fatal(err)
			}
			delivery := run.Acquisition.Delivery
			if delivery.ConnectionCeiling <=
				automaticFitReviewMaximumConnections ||
				delivery.ConnectionLimit != test.wantLimit {
				t.Errorf("delivery = %+v, want raw ceiling above %d and limit %d",
					delivery, automaticFitReviewMaximumConnections,
					test.wantLimit)
			}
		})
	}
}

func TestCappedPathFitWarnsBesideJSON(t *testing.T) {
	t.Parallel()
	const oneWay = 5 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  3,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}), h2tls.WithListenerWrapper(wrapper))
	out, stdout, stderr := captureOutput(t)
	err = runRace(t.Context(), []string{
		"-k", "--report", "json", "--connections-fit", "--batch-rate-max", "1000/s",
		"--connections-fit-max", "2", "--trials", "2", "--arrange", "none",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	var run struct {
		Acquisition struct {
			Delivery structuredDelivery `json:"delivery"`
		} `json:"acquisition"`
	}
	if err := json.NewDecoder(stdout).Decode(&run); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	if run.Acquisition.Delivery.PathFit == nil || !run.Acquisition.Delivery.PathFit.Capped {
		t.Fatalf("delivery = %+v, want capped path fit", run.Acquisition.Delivery)
	}
	diagnostics := strings.Join(strings.Fields(stderr.String()), " ")
	for _, want := range []string{
		diagnosticPrefix(diagnosticWarning),
		"path fit requested ",
		"--connections-fit-max 2 caps the run at 2",
		"may make the requested batch cadence unsustainable",
	} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("stderr omits %q:\n%s", want, stderr)
		}
	}
}
