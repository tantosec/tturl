package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestParsePositiveByteSizeOrUnlimited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value   string
		want    int64
		wantErr bool
	}{
		{value: "1", want: 1},
		{value: "2KiB", want: 2 << 10},
		{value: "3mIb", want: 3 << 20},
		{value: "unlimited"},
		{value: "UNLIMITED"},
		{value: "", wantErr: true},
		{value: "0", wantErr: true},
		{value: "-1", wantErr: true},
		{value: "+1", wantErr: true},
		{value: "1.5MiB", wantErr: true},
		{value: " 1MiB", wantErr: true},
		{value: "1MiB ", wantErr: true},
		{value: "9223372036854775807GiB", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			got, err := parsePositiveByteSizeOrUnlimited(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("value %q parsed as %d", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Errorf("value %q = %d, %v; want %d",
					test.value, got, err, test.want)
			}
		})
	}
}

func TestResponseLimitFlagDefaultsAndPreflight(t *testing.T) {
	t.Parallel()
	for _, command := range []*commandSpec{
		commandByID(commandRace), commandByID(commandMeasure),
		commandByID(commandAnalyse), commandByID(commandDetect),
	} {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()
			layout := newCommand(command)
			var flags *responseLimitFlags
			switch command.id {
			case commandRace:
				flags = configureRaceCommand(layout).responseLimits
			case commandMeasure:
				flags = configureMeasureCommand(layout).responseLimits
			case commandAnalyse:
				flags = configureAnalyseCommand(layout).responseLimits
			case commandDetect:
				flags = configureDetectCommand(layout).responseLimits
			}
			plan, err := layout.parser.Parse([]string{
				"--block", "https://127.0.0.1:1/a",
				"--block", "https://127.0.0.1:1/b",
			})
			if err != nil {
				t.Fatal(err)
			}
			limits, err := flags.resolve(plan.Globals, bodyReleasePlan{})
			if err != nil {
				t.Fatal(err)
			}
			if limits.maxBodyBytes != defaultResponseBodyMax ||
				limits.batchTimeout != defaultBatchTimeout {
				t.Errorf("defaults = %d/%s, want %d/%s",
					limits.maxBodyBytes, limits.batchTimeout,
					defaultResponseBodyMax, defaultBatchTimeout)
			}
		})
	}
}

func TestResponseLimitFlagsResolveFiniteAndUnlimited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		args    []string
		release bodyReleasePlan
		want    responseLimits
	}{
		{
			name: "finite",
			args: []string{
				"--response-body-max", "3GiB", "--batch-timeout", "2.5s",
			},
			want: responseLimits{
				maxBodyBytes: 3 << 30, batchTimeout: 2500 * time.Millisecond,
			},
		},
		{
			name: "unlimited",
			args: []string{
				"--response-body-max", "UNLIMITED",
				"--batch-timeout", "Unlimited",
			},
			release: bodyReleasePlan{releaseDelay: time.Minute},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			layout := newCommand(commandByID(commandMeasure))
			flags := configureMeasureCommand(layout).responseLimits
			args := append(append([]string{}, test.args...),
				"--block", "https://example.test/a",
				"--block", "https://example.test/b")
			plan, err := layout.parser.Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			got, err := flags.resolve(plan.Globals, test.release)
			if err != nil || got != test.want {
				t.Errorf("limits = %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}

func TestResponseLimitValidationPrecedesNetwork(t *testing.T) {
	t.Parallel()
	base := []string{
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "zero body", args: []string{"--response-body-max", "0"},
			want: "--response-body-max",
		},
		{
			name: "zero timeout", args: []string{"--batch-timeout", "0s"},
			want: "--batch-timeout",
		},
		{
			name: "negative timeout", args: []string{"--batch-timeout", "-1s"},
			want: "--batch-timeout",
		},
		{
			name: "overflow timeout", args: []string{"--batch-timeout", "2562048h"},
			want: "--batch-timeout",
		},
		{
			name: "padded timeout", args: []string{"--batch-timeout", " 1s"},
			want: "--batch-timeout",
		},
		{
			name: "unitless timeout", args: []string{"--batch-timeout", "1"},
			want: "--batch-timeout",
		},
		{
			name: "release conflict", args: []string{
				"--batch-timeout", "500us", "--release-delay", "500us",
			},
			want: "must exceed --release-delay (500us)",
		},
		{
			name: "release overflow", args: []string{
				"--release-delay", "2562048h",
			},
			want: "--release-delay",
		},
	}
	for _, command := range []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
	}{
		{name: "race", run: runRace},
		{name: "measure", run: runMeasure},
		{name: "analyse", run: runAnalyse},
		{name: "detect", run: runDetect},
	} {
		for _, test := range tests {
			t.Run(command.name+"/"+test.name, func(t *testing.T) {
				args := append(append([]string{}, test.args...), base...)
				if command.name == "detect" {
					args = append([]string{"--direction", "late"}, args...)
				}
				err := command.run(t.Context(), args, commandOutput{
					stdout: io.Discard, stderr: io.Discard,
				})
				var usageErr *usageError
				if !errors.As(err, &usageErr) ||
					!strings.Contains(err.Error(), test.want) {
					t.Fatalf("error = %v, want usage containing %q",
						err, test.want)
				}
			})
		}
	}
}

func TestResponseLimitReportFormatsDurationsAsASCII(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	reportResponseLimits(&out, responseLimits{
		maxBodyBytes: defaultResponseBodyMax,
		batchTimeout: 500 * time.Microsecond,
	})
	want := "Response limits: 8MiB accepted body per stream; " +
		"500us transport time per batch.\n"
	if got := out.String(); got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
}

func TestDetectResponseLimitDesignValues(t *testing.T) {
	for _, test := range []struct {
		name        string
		flags       []string
		wantBody    int64
		wantTimeout time.Duration
		unlimited   bool
	}{
		{
			name: "defaults", wantBody: defaultResponseBodyMax,
			wantTimeout: defaultBatchTimeout,
		},
		{
			name: "finite",
			flags: []string{
				"--response-body-max", "3KiB", "--batch-timeout", "2s",
			},
			wantBody: 3 << 10, wantTimeout: 2 * time.Second,
		},
		{
			name: "unlimited",
			flags: []string{
				"--response-body-max", "unlimited",
				"--batch-timeout", "unlimited",
			},
			unlimited: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := append([]string{
				"--report", "json", "--direction", "late",
			}, test.flags...)
			args = append(args,
				"--block", "https://127.0.0.1:1/a",
				"--block", "https://127.0.0.1:1/b")
			out, stdout, _ := captureOutput(t)
			err := runDetect(ctx, args, out)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("runDetect error = %v, want cancellation", err)
			}
			lines := jsonLines(t, stdout.Bytes())
			var run jsonDetectRunRecord
			if err := json.Unmarshal(lines[0], &run); err != nil {
				t.Fatal(err)
			}
			limits := run.Acquisition.ResponseLimits
			if test.unlimited {
				if limits.BodyBytes != nil ||
					limits.BatchTimeoutNS != nil {
					t.Errorf("response limits = %+v, want unlimited", limits)
				}
				return
			}
			if limits.BodyBytes == nil ||
				*limits.BodyBytes != test.wantBody ||
				limits.BatchTimeoutNS == nil ||
				*limits.BatchTimeoutNS != test.wantTimeout.Nanoseconds() {
				t.Errorf("response limits = %+v, want %d/%s",
					limits, test.wantBody, test.wantTimeout)
			}
		})
	}
}

func TestRaceJSONRecordsBodyLimitFailureWithoutOutcomes(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = w.Write([]byte("12345"))
	}))
	out, stdout, _ := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--report", "json", "--insecure", "--trials", "1",
		"--response-body-max", "4B", "https://" + addr + "/",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 4 {
		t.Fatalf("records = %d, want run, request, trial, summary: %s",
			len(lines), stdout.String())
	}
	assertResponseLimitRecordsConform(
		t, raceSchema.filename, lines)
	var run jsonRaceRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.Acquisition.ResponseLimits.BodyBytes == nil ||
		*run.Acquisition.ResponseLimits.BodyBytes != 4 ||
		run.Acquisition.ResponseLimits.BatchTimeoutNS == nil {
		t.Errorf("response limits = %+v", run.Acquisition.ResponseLimits)
	}
	var trial jsonRaceTrialRecord
	if err := json.Unmarshal(lines[2], &trial); err != nil {
		t.Fatal(err)
	}
	if trial.Failure == nil ||
		trial.Failure.Code != string(failureResponseBodyLimit) ||
		len(trial.Outcomes) != 1 ||
		trial.Outcomes[0].Response != nil || trial.Outcomes[0].Rank != nil {
		t.Errorf("failed trial = %+v", trial)
	}
}

func TestRaceBodyLimitCoversPrimingAndReplacement(t *testing.T) {
	t.Parallel()
	var warmups atomic.Int32
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/warm" && warmups.Add(1) > 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("12345"))
	}))
	out, stdout, _ := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--report", "json", "--insecure", "--trials", "2",
		"--warmup", "1", "--connections", "1", "--arrange", "none",
		"--response-body-max", "4B", "--capture-body", "unlimited",
		"--block", "https://" + addr + "/warm", "--warmup-only",
		"--block", "https://" + addr + "/measure",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	assertResponseLimitRecordsConform(
		t, raceSchema.filename, lines)
	localFailures, successfulWarmups := 0, 0
	connections := map[int]bool{}
	for _, line := range lines {
		if recordKind(t, line) != "trial" && recordKind(t, line) != "warmup" {
			continue
		}
		var trial jsonRaceTrialRecord
		if err := json.Unmarshal(line, &trial); err != nil {
			t.Fatal(err)
		}
		connections[trial.ConnectionID] = true
		if trial.Failure == nil {
			if recordKind(t, line) == "warmup" {
				successfulWarmups++
			}
			continue
		}
		localFailures++
		if trial.Failure.Code != string(failureResponseBodyLimit) ||
			trial.Failure.Message == "" {
			t.Errorf("failure = %+v", trial)
		}
		for _, outcome := range trial.Outcomes {
			if outcome.Response != nil || outcome.Rank != nil {
				t.Errorf("failed batch exposed outcome %+v", outcome)
			}
		}
	}
	if localFailures != 3 || successfulWarmups != 2 || len(connections) != 3 {
		t.Errorf("failures/successful warmups/connections = %d/%d/%d",
			localFailures, successfulWarmups, len(connections))
	}
	var summary jsonRaceResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Execution.Trials.RankComplete != 0 ||
		summary.Execution.Trials.Incomplete != 2 ||
		summary.Execution.Priming.Batches.Attempted != 3 ||
		summary.Execution.Priming.Batches.RankComplete != 2 ||
		summary.Execution.Priming.Batches.Incomplete != 1 {
		t.Errorf("summary = %+v", summary)
	}
}

func assertResponseLimitRecordsConform(
	t *testing.T,
	schemaName string,
	records [][]byte,
) {
	t.Helper()
	schema := compileStructuredSchema(t, schemaName)
	for i, record := range records {
		var instance any
		if err := json.Unmarshal(record, &instance); err != nil {
			t.Fatalf("decode record %d: %v", i, err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("record %d does not conform: %v\n%s", i, err, record)
		}
	}
}

func TestMeasureJSONRecordsTerminalPrimingTimeoutsWithoutMeasurements(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		_ http.ResponseWriter,
		r *http.Request,
	) {
		<-r.Context().Done()
	}))
	out, stdout, _ := captureOutput(t)
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure", "--trials", "4",
		"--arrange", "none", "--connections", "1",
		"--warmup", "1",
		"--batch-timeout", "40ms", "--response-body-max", "unlimited",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if failure := classifyBatchFailure(err); failure == nil ||
		failure.Code != failureBatchTimeout {
		t.Fatalf("runMeasure error = %v, want batch timeout", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	assertResponseLimitRecordsConform(
		t, measureSchema.filename, lines)
	var run jsonMeasureRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.Acquisition.ResponseLimits.BodyBytes != nil ||
		run.Acquisition.ResponseLimits.BatchTimeoutNS == nil ||
		*run.Acquisition.ResponseLimits.BatchTimeoutNS !=
			(40*time.Millisecond).Nanoseconds() {
		t.Errorf("response limits = %+v", run.Acquisition.ResponseLimits)
	}
	var summary jsonMeasureResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Measurement.State != "unavailable" ||
		summary.Execution.Trials.Attempted != 0 ||
		summary.Execution.Trials.Incomplete != 0 ||
		summary.Execution.Trials.Unattempted != 4 ||
		summary.Execution.Trials.Retained != 0 ||
		summary.Completion.State != completionFailed ||
		summary.Execution.Priming.Batches.Attempted != 2 ||
		summary.Execution.Priming.Batches.Incomplete != 2 ||
		summary.Execution.Connections.Observed != 2 ||
		summary.Execution.Connections.Replacements != 1 ||
		len(summary.AcquisitionFailures) != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	for _, failure := range summary.AcquisitionFailures {
		if failure.Failure.Code != string(failureBatchTimeout) {
			t.Errorf("failure = %+v", failure)
		}
	}
	for _, request := range summary.Measurement.Requests {
		if request.Samples != 0 || request.MeanRank != nil ||
			len(request.StatusCounts) != 0 {
			t.Errorf("request measurement escaped = %+v", request)
		}
	}
}

func TestAnalyseResponseLimitsMakeInferenceUnavailable(t *testing.T) {
	for _, test := range []struct {
		name    string
		priming bool
		code    batchFailureCode
		handler http.HandlerFunc
		limit   []string
	}{
		{
			name: "measured body", code: failureResponseBodyLimit,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
			limit: []string{"--response-body-max", "4B"},
		},
		{
			name: "priming body", priming: true,
			code: failureResponseBodyLimit,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
			limit: []string{"--response-body-max", "4B"},
		},
		{
			name: "measured timeout", code: failureBatchTimeout,
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
			limit: []string{"--batch-timeout", "10ms"},
		},
		{
			name: "priming timeout", priming: true,
			code: failureBatchTimeout,
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
			limit: []string{"--batch-timeout", "10ms"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr, _ := h2test.Serve(t, test.handler)
			args := []string{
				"--report", "json", "--insecure", "--cycles", "6",
				"--connections", "1",
			}
			args = append(args, test.limit...)
			if test.priming {
				args = append(args,
					"--warmup", "1",
					"--block", "https://"+addr+"/warm", "--warmup-only")
			} else {
				args = append(args, "--warmup", "0")
			}
			args = append(args,
				"--block", "https://"+addr+"/a",
				"--block", "https://"+addr+"/b")
			out, stdout, _ := captureOutput(t)
			err := runAnalyse(t.Context(), args, out)
			if !errors.Is(err, errAnalyseUnavailable) {
				t.Fatalf("runAnalyse error = %v, want unavailable", err)
			}
			lines := jsonLines(t, stdout.Bytes())
			assertResponseLimitRecordsConform(
				t, analyseSchema.filename, lines)
			failures, terminalPriming := 0, false
			for _, line := range lines {
				switch recordKind(t, line) {
				case "trial":
					var trial jsonAnalyseTrial
					if err := json.Unmarshal(line, &trial); err != nil {
						t.Fatal(err)
					}
					if trial.Failure == nil {
						continue
					}
					failures++
					if trial.Failure.Code != string(test.code) || trial.Failure.Message == "" {
						t.Errorf("trial failure = %+v", trial)
					}
					for _, outcome := range trial.Outcomes {
						if outcome.Response != nil || outcome.Rank != nil {
							t.Errorf("local failure exposed response facts: %+v", outcome)
						}
					}
				case "result":
					var result jsonAnalyseResultRecord
					if err := json.Unmarshal(line, &result); err != nil {
						t.Fatal(err)
					}
					if result.Analysis.State != "unavailable" || result.Analysis.Reason == nil {
						t.Errorf("terminal inference = %+v", result.Analysis)
					}
					if test.priming {
						terminalPriming = result.Completion.State == completionFailed &&
							result.Execution.Priming.Batches.Attempted == 2 &&
							result.Execution.Priming.Batches.Incomplete == 2 &&
							result.Execution.Trials.Attempted == 0
					}
				}
			}
			if test.priming && (!terminalPriming || failures != 0) {
				t.Fatalf("priming termination/failures = %t/%d, want true/0",
					terminalPriming, failures)
			}
			if !test.priming && failures == 0 {
				t.Fatal("analyse stream has no classified local failure")
			}
			replayed, err := replayAnalyseJSONStream(
				strings.NewReader(stdout.String()))
			if err != nil {
				t.Fatalf("replay unavailable stream: %v", err)
			}
			if replayed.Global.Outcome != analyseUnavailable {
				t.Fatalf("replayed outcome = %q", replayed.Global.Outcome)
			}
		})
	}
}

func TestDetectResponseLimitsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		priming  bool
		parallel int
		code     batchFailureCode
		limit    []string
		handler  http.HandlerFunc
	}{
		{
			name: "serial comparison body limit", parallel: 1,
			code:  failureResponseBodyLimit,
			limit: []string{"--response-body-max", "4B"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
		},
		{
			name: "concurrent comparison body limit", parallel: 2,
			code:  failureResponseBodyLimit,
			limit: []string{"--response-body-max", "4B"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
		},
		{
			name: "serial comparison timeout", parallel: 1,
			code:  failureBatchTimeout,
			limit: []string{"--batch-timeout", "10ms"},
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		},
		{
			name: "concurrent comparison timeout", parallel: 2,
			code:  failureBatchTimeout,
			limit: []string{"--batch-timeout", "10ms"},
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		},
		{
			name: "serial priming body limit", priming: true, parallel: 1,
			code:  failureResponseBodyLimit,
			limit: []string{"--response-body-max", "4B"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
		},
		{
			name: "concurrent priming body limit", priming: true, parallel: 2,
			code:  failureResponseBodyLimit,
			limit: []string{"--response-body-max", "4B"},
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "12345")
			},
		},
		{
			name: "concurrent priming timeout", priming: true, parallel: 2,
			code:  failureBatchTimeout,
			limit: []string{"--batch-timeout", "10ms"},
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		},
		{
			name: "serial priming timeout", priming: true, parallel: 1,
			code:  failureBatchTimeout,
			limit: []string{"--batch-timeout", "10ms"},
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var handled atomic.Int32
			addr, _ := h2test.Serve(t, http.HandlerFunc(func(
				w http.ResponseWriter, r *http.Request,
			) {
				handled.Add(1)
				test.handler(w, r)
			}))
			args := []string{
				"--report", "json", "--insecure", "--direction", "late",
				"--comparisons-max", "10", "--connections",
				strconv.Itoa(test.parallel),
			}
			args = append(args, test.limit...)
			if test.priming {
				args = append(args,
					"--warmup", "1", "--block",
					"https://"+addr+"/prime", "--warmup-only")
			}
			args = append(args,
				"--block", "https://"+addr+"/a",
				"--block", "https://"+addr+"/b")
			out, stdout, _ := captureOutput(t)
			err := runDetect(t.Context(), args, out)
			if err == nil {
				t.Fatal("runDetect succeeded after a local response limit")
			}
			lines := jsonLines(t, stdout.Bytes())
			assertResponseLimitRecordsConform(
				t, detectSchema.filename, lines)
			var run jsonDetectRunRecord
			if err := json.Unmarshal(lines[0], &run); err != nil {
				t.Fatal(err)
			}
			if test.code == failureResponseBodyLimit &&
				(run.Acquisition.ResponseLimits.BodyBytes == nil ||
					*run.Acquisition.ResponseLimits.BodyBytes != 4) {
				t.Errorf("response limits = %+v", run.Acquisition.ResponseLimits)
			}
			if test.code == failureBatchTimeout &&
				(run.Acquisition.ResponseLimits.BatchTimeoutNS == nil ||
					*run.Acquisition.ResponseLimits.BatchTimeoutNS !=
						(10*time.Millisecond).Nanoseconds()) {
				t.Errorf("response limits = %+v", run.Acquisition.ResponseLimits)
			}
			var result jsonDetectResultRecord
			if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
				t.Fatal(err)
			}
			if result.Detection.State != "unavailable" ||
				result.Execution.Comparisons.RankComplete != 0 ||
				result.Execution.RequestOperations.Retained != 0 ||
				result.Execution.Connections.Replacements != 0 ||
				result.Completion.State != completionFailed {
				t.Errorf("terminal result = %+v", result)
			}
			failures := 0
			if test.priming {
				for _, attempt := range result.PrimingAttempts {
					if attempt.Failure != nil {
						if attempt.Failure.Code == string(test.code) {
							failures++
						}
						if attempt.Failure.Code != string(test.code) && attempt.Failure.Code != "interrupted" ||
							attempt.Failure.Message == "" {
							t.Errorf("priming failure = %+v", attempt)
						}
					}
				}
				if len(result.ComparisonFailures) != 0 {
					t.Errorf("priming failure sent comparisons: %+v",
						result.ComparisonFailures)
				}
			} else {
				for _, failure := range result.ComparisonFailures {
					if failure.Failure != nil {
						if failure.Failure.Code == string(test.code) {
							failures++
						}
						if failure.Failure.Code != string(test.code) && failure.Failure.Code != "interrupted" ||
							failure.Failure.Message == "" {
							t.Errorf("comparison failure = %+v", failure)
						}
					}
				}
			}
			maxHandled := int32(2)
			if test.parallel == 2 {
				maxHandled = 4
			}
			if failures == 0 || handled.Load() > maxHandled {
				t.Errorf("classified failures/handled requests = %d/%d",
					failures, handled.Load())
			}
		})
	}
}
