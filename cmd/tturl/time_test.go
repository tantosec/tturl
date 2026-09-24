package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/timing"
)

func parseTimeFlagsForTest(t *testing.T, args ...string) (timeCommandConfig, error) {
	t.Helper()
	layout := newCommand(commandByID(commandRace))
	flags := configureTimeCommand(layout)
	plan, err := layout.parser.Parse(append(args, "https://one.test/"))
	if err != nil {
		return timeCommandConfig{}, err
	}
	return flags.resolve(plan)
}

func TestTimeCommandDefaults(t *testing.T) {
	cfg, err := parseTimeFlagsForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.trials != 1 || cfg.arrangement != arrangeNone || cfg.warmup != 0 || cfg.connectionsSet ||
		cfg.synchronise || !cfg.singleRecord || cfg.lastByte || cfg.releaseDelay != 0 ||
		cfg.requestTimeout != 30*time.Second || cfg.runTimeout != 0 || cfg.bodyMax != 8<<20 ||
		cfg.batchRate != 0 || cfg.requestRate != 0 || cfg.report.format != reportText {
		t.Fatalf("defaults=%+v", cfg)
	}
	cfg, err = parseTimeFlagsForTest(t, "--trials", "unlimited", "--request-timeout", "unlimited",
		"--response-body-max", "unlimited")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.trials != 0 || cfg.requestTimeout != 0 || cfg.bodyMax != 0 || cfg.arrangement != arrangeRandom {
		t.Fatalf("unlimited=%+v", cfg)
	}
}

func TestTimeCommandApplicability(t *testing.T) {
	for _, args := range [][]string{
		{"--release-delay", "1ms"},
		{"--last-byte-sync", "--release-delay", "1s", "--request-timeout", "1s"},
		{"--request-timeout", "0s"},
		{"--trials", "0"},
		{"--dry-run", "--report", "json"},
		{"--capture-headers"},
		{"--extract-regex", "x"},
		{"--body-bytes-withheld", "1"},
		{"--ignore-peer-stream-limit"},
		{"--padding", "1"},
		{"--batch-timeout", "1s"},
	} {
		if _, err := parseTimeFlagsForTest(t, args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	cfg, err := parseTimeFlagsForTest(t, "--connections", "8", "--synchronise", "--single-record=false",
		"--last-byte-sync", "--release-delay", "1ms", "--batch-rate-max", "2/m", "--request-rate-max", "3/s")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.connections != 8 || !cfg.connectionsSet || !cfg.synchronise || cfg.singleRecord ||
		!cfg.lastByte || cfg.releaseDelay != time.Millisecond || cfg.batchRate != 2.0/60 || cfg.requestRate != 3 {
		t.Fatalf("policies=%+v", cfg)
	}
}

func TestTimePlatformStartup(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--dry-run"},
		{"--version"},
		{"-V"},
		{"--data-binary", "@-"},
		{"--output", "report.json"},
		{"-H", "-h"},
		{"--name", "-h"},
		{"--", "-h"},
	} {
		if err := timeStartupAllowed("windows", args); err == nil {
			t.Fatalf("Windows allowed %v", args)
		}
	}
	for _, args := range [][]string{{"-h"}, {"--help"}, {"--data-binary", "@missing", "--help"}} {
		if err := timeStartupAllowed("windows", args); err != nil {
			t.Fatal(err)
		}
	}
	for _, platform := range []string{"linux", "darwin"} {
		if err := timeStartupAllowed(platform, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimeCommandPlanTranslation(t *testing.T) {
	cfg, err := parseTimeFlagsForTest(t, "--trials", "unlimited", "--single-record=false", "--last-byte-sync",
		"--request-timeout", "unlimited", "--response-body-max", "unlimited",
		"--report", "json", "--capture-body", "unlimited", "--capture-headers")
	if err != nil {
		t.Fatal(err)
	}
	plan := cfg.enginePlan(timeRequests{}, true)
	if plan.Trials != 0 || plan.Connections != 0 || plan.SingleRecord || !plan.LastByteSync ||
		plan.RequestTimeout != 0 || plan.ResponseBodyMax != -1 || plan.ReceiveHeaderMax != 1<<20 ||
		!plan.Capture.Body || plan.Capture.BodyMax != -1 || !plan.Capture.Headers || !plan.TLSConfig.InsecureSkipVerify {
		t.Fatalf("engine policies=%+v", plan)
	}
	cfg, err = parseTimeFlagsForTest(t, "--connections", "4")
	if err != nil {
		t.Fatal(err)
	}
	if plan := cfg.enginePlan(timeRequests{}, false); plan.Connections != 4 || plan.Trials != 1 ||
		!plan.SingleRecord || plan.RequestTimeout != 30*time.Second {
		t.Fatalf("engine policies=%+v", plan)
	}
}

func TestTimeCommandResolvedAllocation(t *testing.T) {
	for _, item := range []struct {
		name                                   string
		controls                               []string
		ceiling, perWorker, workers, effective int
		planned                                uint64
	}{
		{"sequential derived", nil, 2, 2, 1, 2, 1},
		{"synchronised derived", []string{"--synchronise"}, 3, 3, 1, 3, 1},
		{"whole trial workers", []string{"--trials", "4", "--connections", "5"}, 5, 2, 2, 4, 4},
		{"whole rotation workers", []string{"--trials", "4", "--arrange", "rotate", "--connections", "9"}, 9, 2, 2, 4, 6},
		{"bounded synchronised workers", []string{"--connections", "9", "--synchronise"}, 9, 3, 1, 3, 1},
	} {
		t.Run(item.name, func(t *testing.T) {
			args := append(append([]string{}, item.controls...),
				"--block", "https://one.test/a", "--repeat", "2", "--block", "https://two.test/b")
			resolved, err := resolveTimeArgsForTest(t, args)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Width != 3 || resolved.Destinations != 2 || resolved.ConnectionCeiling != item.ceiling ||
				resolved.ConnectionsPerWorker != item.perWorker || resolved.Workers != item.workers ||
				resolved.EffectiveConnections != item.effective || resolved.PlannedTrials != item.planned {
				t.Fatalf("allocation=%+v", resolved)
			}
		})
	}
}

func TestTimeCommandPlanApplicability(t *testing.T) {
	for _, args := range [][]string{
		{"--connections", "1", "--block", "https://one.test/", "--block", "https://two.test/"},
		{"--synchronise", "--connections", "1", "--repeat", "2", "https://one.test/"},
		{"--http1.1", "http://one.test/"},
		{"--single-record=false", "http://one.test/"},
		{"--http1.1", "--single-record=false", "--last-byte-sync", "http://one.test/"},
		{"--warmup", "1", "--block", "https://one.test/", "--block", "https://two.test/", "--warmup-only"},
		{"--warmup", "1", "--block", "https://one.test/", "--block", "https://one.test/", "--warmup-only", "--http1.1"},
		{"--block", "https://one.test/", "--single-record=false"},
	} {
		if _, err := resolveTimeArgsForTest(t, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := resolveTimeArgsForTest(t, []string{
		"--http1.1", "--single-record=false", "http://one.test/",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTimeArgsForTest(t, []string{
		"--warmup", "1", "--block", "https://one.test/",
		"--block", "https://one.test/prime", "--warmup-only",
	}); err != nil {
		t.Fatal(err)
	}
}

func resolveTimeArgsForTest(t *testing.T, args []string) (*timing.Resolved, error) {
	t.Helper()
	layout := newCommand(commandByID(commandRace))
	flags := configureTimeCommand(layout)
	plan, err := layout.parser.Parse(args)
	if err != nil {
		return nil, err
	}
	cfg, err := flags.resolve(plan)
	if err != nil {
		return nil, err
	}
	requests, err := materialiseTimeRequests(context.Background(), plan, flags.protocols, flags.roles)
	if err != nil {
		return nil, err
	}
	return timing.Resolve(cfg.enginePlan(requests, plan.Globals.Insecure))
}

func TestTimeSilentSelectionUsesSharedSwitchGrammar(t *testing.T) {
	spec := commandByID(commandTime)
	for _, tc := range []struct {
		args            []string
		silent, wantErr bool
	}{
		{args: []string{"-s"}, silent: true},
		{args: []string{"--silent"}, silent: true},
		{args: []string{"-vs"}, silent: true},
		{args: []string{"-H", "--silent"}},
		{args: []string{"--name", "-s"}},
		{args: []string{"--", "--silent"}},
		{args: []string{"--silent=true"}, wantErr: true},
		{args: []string{"--silent=false"}, wantErr: true},
		{args: []string{"-s=true"}, wantErr: true},
		{args: []string{"-vs=false"}, wantErr: true},
		{args: []string{"-s", "--silent=false"}, silent: true, wantErr: true},
	} {
		var diagnostics bytes.Buffer
		out, err := commandDiagnosticOutput(spec, tc.args, commandOutput{stderr: &diagnostics})
		if (err != nil) != tc.wantErr || (out.stderr == io.Discard) != tc.silent {
			t.Fatalf("%v: silent %t error %v", tc.args, out.stderr == io.Discard, err)
		}
	}
}
