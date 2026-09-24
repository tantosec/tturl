package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
)

func TestTimeCompactPreviewSharesRequestSummary(t *testing.T) {
	var output bytes.Buffer
	err := runTimeCommandForPlatform(t.Context(), commandByID(commandTime), []string{
		"--dry-run", "--trials", "3", "--arrange", "rotate", "-k", "--block", "https://example.test/a", "--name", "first",
		"-H", "X-Case: one", "--block", "https://example.test/b", "--name", "second", "-H", "X-Case: two",
	}, commandOutput{stdout: &output, stderr: &bytes.Buffer{}}, "linux",
		func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
			t.Fatal("preview reached engine")
			return timing.Result{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		"tturl time", "Preview: network-free dry run", "Requests", "Target: https://example.test",
		"first", "second", "/a", "/b", "X-Case: one", "X-Case: two", "Total: 2 requests.",
		"TLS: certificate verification disabled (--insecure).", "8MiB", "1MiB",
		"Trials: 4 x 2 requests = 8 request operations.",
		"Trial allocation: 3 requested, 4 planned.", "Arrangement: rotate.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ":method:") || strings.Contains(text, "user-agent:") {
		t.Fatalf("compact preview expanded wire headers:\n%s", text)
	}
	if strings.Index(text, "Preview:") > strings.Index(text, "Requests") {
		t.Fatal("preview qualification must precede requests")
	}
}

func TestTimeDurationSummaryUsesLocationScaleForDispersion(t *testing.T) {
	var output bytes.Buffer
	reporter := newTimeReporter(newReportWriter(&output), timeCommandConfig{}, timeRequests{
		Measured: []timeRequest{
			{ID: 0, Label: "first", Protocol: curlblocks.HTTP2},
			{ID: 1, Label: "second", Protocol: curlblocks.HTTP2},
		},
	}, nil, buildinfo.Info{}, nil).(*timeReport)
	reporter.samples[0] = []int64{99_950_000, 100_050_000}
	reporter.samples[1] = []int64{199_950_000, 200_050_000}
	if err := reporter.Summary(timing.Result{Completion: timing.CompletionComplete}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		"Initial release to valid final response headers.", "Request 0: first", "Request 1: second",
		"mean 100ms  standard deviation 0.05ms",
		"mean 200ms  standard deviation 0.05ms",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "e+") || strings.Contains(text, "e-") {
		t.Fatalf("scientific duration notation:\n%s", text)
	}
}

func TestTimeDurationUnitIgnoresZeroAndSharesFixedNotation(t *testing.T) {
	for _, test := range []struct {
		values []int64
		name   string
	}{
		{nil, "ns"},
		{[]int64{0, 0}, "ns"},
		{[]int64{0, 100}, "ns"},
		{[]int64{0, 100_000}, "us"},
		{[]int64{0, 100_000_000}, "ms"},
		{[]int64{2_000_000_000}, "s"},
	} {
		unit := timeDurationUnit(map[int][]int64{0: test.values})
		if unit.name != test.name {
			t.Fatalf("values=%v unit=%s", test.values, unit.name)
		}
		if unit.stat(nil) != "-" {
			t.Fatal("absent statistic must remain absent")
		}
		value := 1e15
		if strings.ContainsAny(unit.stat(&value), "eE") {
			t.Fatal("duration formatting used scientific notation")
		}
	}
}

func TestTimeDurationValuesTruncateWithAdjacentUnitSuffixes(t *testing.T) {
	unit := timeTextUnit{"ms", 1e6}
	for _, test := range []struct {
		value float64
		want  string
	}{
		{100_000_000, "100ms"},
		{100_200_000, "100.2ms"},
		{100_250_000, "100.25ms"},
		{200_510_000, "200.51ms"},
		{100_999_999, "100.999ms"},
		{-100_999_999, "-100.999ms"},
		{50_000, "0.05ms"},
		{999, "0ms"},
		{-999, "0ms"},
	} {
		if actual := unit.stat(&test.value); actual != test.want {
			t.Fatalf("value=%v got=%q want=%q", test.value, actual, test.want)
		}
	}
	large := math.MaxFloat64
	text := (timeTextUnit{"ns", 1}).stat(&large)
	if strings.ContainsAny(text, "eE") || !strings.HasSuffix(text, "ns") {
		t.Fatalf("large finite duration=%q", text)
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(text, "ns")), 64); err != nil {
		t.Fatalf("large finite duration lost its numeric representation: %v", err)
	}
}

func TestTimeDurationIntegerExtremaRemainExact(t *testing.T) {
	for _, value := range []int64{math.MinInt64, math.MaxInt64} {
		if actual := (timeTextUnit{"ns", 1}).integer(&value); actual != strconv.FormatInt(value, 10)+"ns" {
			t.Fatalf("integer duration=%q", actual)
		}
	}
	value := int64(math.MaxInt64)
	if actual := (timeTextUnit{"ms", 1e6}).integer(&value); actual != "9223372036854.775ms" {
		t.Fatalf("scaled integer duration=%q", actual)
	}
	if actual := (timeTextUnit{"ms", 1e6}).integer(nil); actual != "-" {
		t.Fatalf("missing duration=%q", actual)
	}
}

func TestTimeSummaryUsesCompactColumnsAndAdjacentUnits(t *testing.T) {
	var output bytes.Buffer
	reporter := newTimeReporter(newReportWriter(&output), timeCommandConfig{}, timeRequests{
		Measured: []timeRequest{
			{ID: 0, Label: "first", Protocol: curlblocks.HTTP2},
			{ID: 1, Label: "second", Protocol: curlblocks.HTTP2},
			{ID: 2, Label: "missing", Protocol: curlblocks.HTTP2},
		},
	}, nil, buildinfo.Info{}, nil).(*timeReport)
	reporter.samples[0] = []int64{100_234_567, 200_789_123}
	reporter.samples[1] = []int64{1_000_234_567, 2_000_789_123}
	if err := reporter.Summary(timing.Result{Completion: timing.CompletionComplete}); err != nil {
		t.Fatal(err)
	}
	value := `(-|[0-9]+(?:\.[0-9]{1,3})?ms)`
	percentiles := regexp.MustCompile(`^  (min|p95) +` + value +
		`   (p50|p99) +` + value + `   (p90|max) +` + value + `$`)
	moments := regexp.MustCompile(`^  mean ` + value + `  standard deviation ` + value + `$`)
	rows, meanRows := 0, 0
	missing := false
	for line := range strings.SplitSeq(output.String(), "\n") {
		if strings.HasPrefix(line, "  mean ") {
			meanRows++
			if !moments.MatchString(line) {
				t.Fatalf("mean and standard deviation spacing or suffix: %q", line)
			}
			continue
		}
		if !strings.HasPrefix(line, "  min ") && !strings.HasPrefix(line, "  p95 ") {
			continue
		}
		rows++
		if len(line) > textWidth {
			t.Fatalf("duration row exceeds width: %q", line)
		}
		if !percentiles.MatchString(line) {
			t.Fatalf("percentile column spacing or suffix: %q", line)
		}
		if strings.Contains(line, "-") {
			missing = true
			if strings.Contains(line, "ms") {
				t.Fatalf("missing statistic has a unit: %q", line)
			}
		}
	}
	if rows != 6 || meanRows != 3 || !missing {
		t.Fatalf("summary rows=%d mean rows=%d missing=%t", rows, meanRows, missing)
	}
}

func TestTimePreviewPlannedInitialWarmupCosts(t *testing.T) {
	for _, test := range []struct {
		name                                            string
		args                                            []string
		workers, connections, width, trials, operations int
	}{
		{"sequential", []string{"--warmup", "2", "--repeat", "3"}, 1, 1, 3, 2, 6},
		{"synchronised", []string{"--warmup", "2", "--synchronise", "--repeat", "3"}, 1, 3, 3, 2, 6},
		{"four workers", []string{
			"--warmup", "10", "--synchronise", "--repeat", "128",
			"--connections", "512", "--trials", "4", "--arrange", "none",
		}, 4, 512, 128, 40, 5120},
		{"narrowed", []string{
			"--warmup", "2", "--repeat", "3", "--connections", "4",
			"--trials", "2", "--arrange", "none",
		}, 2, 2, 3, 4, 12},
		{"zero", []string{"--repeat", "3"}, 1, 1, 3, 0, 0},
		{"mixed pools", []string{
			"--warmup", "2", "--block", "https://fixture.test/a",
			"--http1.1", "--block", "https://fixture.test/b",
		}, 1, 2, 2, 2, 4},
		{"distinct width", []string{
			"--warmup", "2", "--block", "https://fixture.test/w", "--warmup-only",
			"--repeat", "2", "--block", "https://fixture.test/m",
		}, 1, 1, 2, 2, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--dry-run"}, test.args...)
			if !slices.Contains(test.args, "--block") {
				args = append(args, "https://fixture.test/a")
			}
			var output bytes.Buffer
			err := runTimeCommandForPlatform(t.Context(), commandByID(commandTime), args,
				commandOutput{stdout: &output, stderr: &bytes.Buffer{}}, "linux",
				func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
					t.Fatal("dry-run dispatched live work")
					return timing.Result{}, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				fmt.Sprintf("workers: %d.", test.workers),
				fmt.Sprintf("Connections: %d effective", test.connections),
				fmt.Sprintf("width: %d.", test.width),
				fmt.Sprintf("Planned initial warmup: %d trials; %d request operations.", test.trials, test.operations),
			} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("missing %q:\n%s", want, output.String())
				}
			}
		})
	}
}
