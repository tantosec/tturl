package main

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

// TestBannerHeading checks the root and section fencing geometry.
func TestBannerHeading(t *testing.T) {
	rule := strings.Repeat("=", textWidth)
	lines := strings.Split(strings.TrimRight(banner("Title"), "\n"), "\n")
	if len(lines) != 3 || lines[0] != rule || lines[2] != rule {
		t.Errorf("banner not fenced by %d-wide '=' rules:\n%s", textWidth, banner("Title"))
	}
	if !strings.Contains(lines[1], "Title") {
		t.Errorf("banner title line missing the title: %q", lines[1])
	}

	if h := heading("Requests"); len(h) != textWidth || !strings.Contains(h, " Requests ") {
		t.Errorf("heading = %q (len %d), want width %d around ' Requests '", h, len(h), textWidth)
	}
}

func TestReportHeadingSpacing(t *testing.T) {
	var out bytes.Buffer
	w := newReportWriter(&out)
	writeBanner(w, "Title")
	writeHeading(w, "First")
	emitln(w, "body")
	writeHeading(w, "Second")
	emitln(w, "more body")
	emitln(w)
	writeHeading(w, "Third")

	want := banner("Title") + "\n" +
		heading("First") + "\n\nbody\n\n" +
		heading("Second") + "\n\nmore body\n\n" +
		heading("Third") + "\n\n"
	if out.String() != want {
		t.Fatalf("report layout:\n%q\nwant:\n%q", out.String(), want)
	}
}

// TestLabelColumn checks the column width: a floor of 6, widened to the longest
// label so every bar in a table starts at the same column.
func TestLabelColumn(t *testing.T) {
	if got := labelColumn(nil); got != 6 {
		t.Errorf("labelColumn(nil) = %d, want the floor 6", got)
	}
	if got := labelColumn([]string{"AB", "CDE"}); got != 6 {
		t.Errorf("labelColumn(short) = %d, want the floor 6", got)
	}
	if got := labelColumn([]string{"A", "abcdefghij"}); got != 10 {
		t.Errorf("labelColumn(long) = %d, want 10 (the longest label)", got)
	}
}

func TestCompactRate(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{math.NaN(), "--"}, // no rate known yet
		{0, "--"},
		{-5, "--"}, // never a real rate; render as unknown
		{0.5, "0.50"},
		{0.067, "0.067"},
		{0.00001, "0.000010"},
		{0.0000001, "<0.000001"},
		{50, "50.0"},
		{99.9, "99.9"},
		{100, "100"},   // switches to no-decimal at 100
		{640, "640"},   // typical cmp/s
		{1234, "1.2k"}, // k above a thousand
		{5120, "5.1k"}, // typical participations/s
		{2_500_000, "2.5M"},
	}
	for _, c := range cases {
		if got := compactRate(c.v); got != c.want {
			t.Errorf("compactRate(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestSustainablePace(t *testing.T) {
	tests := []struct {
		name                    string
		work, requests          float64
		workLimit, requestLimit float64
		want                    paceDisplay
	}{
		{
			name: "initial burst is constrained",
			work: 439, requests: 439, requestLimit: 2.0 / 3600,
			want: paceDisplay{work: "2.0", requests: "2.0", unit: "h"},
		},
		{
			name: "request cost scales work pace",
			work: 100, requests: 400, requestLimit: 2.0 / 3600,
			want: paceDisplay{work: "0.50", requests: "2.0", unit: "h"},
		},
		{
			name: "batch ceiling scales request pace",
			work: 100, requests: 400, workLimit: 10,
			want: paceDisplay{work: "10.0", requests: "40.0", unit: "s"},
		},
		{
			name: "observed slowdown passes through",
			work: 0.25, requests: 0.5, requestLimit: 1,
			want: paceDisplay{work: "15.0", requests: "30.0", unit: "m"},
		},
		{
			name: "small work rate retains a fraction",
			work: 1.0 / 864000, requests: 1.0 / 86400,
			want: paceDisplay{work: "0.10", requests: "1.0", unit: "d"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sustainablePace(
				test.work, test.requests,
				test.workLimit, test.requestLimit,
			).display()
			if got != test.want {
				t.Errorf("display = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestCompactETA(t *testing.T) {
	tests := []struct {
		name string
		p    tth2.TrialProgress
		eta  trialETA
		want string
	}{
		{
			name: "seconds round up",
			p: tth2.TrialProgress{
				Trials: 18, RecentTrialsPerSecond: 6,
			},
			eta:  trialETA{limit: 30},
			want: "2s",
		},
		{
			name: "subsecond remains nonzero",
			p: tth2.TrialProgress{
				Trials: 29, RecentTrialsPerSecond: 20,
			},
			eta:  trialETA{limit: 30},
			want: "1s",
		},
		{
			name: "tens round to five seconds",
			p: tth2.TrialProgress{
				Trials: 10, RecentTrialsPerSecond: 3,
			},
			eta:  trialETA{limit: 50},
			want: "15s",
		},
		{
			name: "minutes round to ten seconds",
			p: tth2.TrialProgress{
				Trials: 20, RecentTrialsPerSecond: 1,
			},
			eta:  trialETA{limit: 105},
			want: "1m30s",
		},
		{
			name: "long estimates round up to ten seconds",
			p: tth2.TrialProgress{
				Trials: 20, RecentTrialsPerSecond: 1,
			},
			eta:  trialETA{limit: 641},
			want: "10m30s",
		},
		{
			name: "long estimates use the minute handling",
			p: tth2.TrialProgress{
				Trials: 20, RecentTrialsPerSecond: 1,
			},
			eta:  trialETA{limit: 914},
			want: "15m0s",
		},
		{
			name: "request pacing bounds the initial burst",
			p: tth2.TrialProgress{
				Trials: 1, RecentTrialsPerSecond: 446,
				RecentRequestsPerSecond: 446,
			},
			eta: trialETA{
				limit: 10, requestsPerTrial: 1,
				maxRequestsPerSecond: 10.0 / 3600,
			},
			want: "54m0s",
		},
		{
			name: "batch width consumes request capacity",
			p: tth2.TrialProgress{
				Trials: 10, RecentTrialsPerSecond: 20,
				RecentRequestsPerSecond: 80,
			},
			eta: trialETA{
				limit: 20, requestsPerTrial: 4,
				maxRequestsPerSecond: 8,
			},
			want: "5s",
		},
		{
			name: "batch pacing supplies an estimate before throughput",
			p: tth2.TrialProgress{
				RecentTrialsPerSecond: math.NaN(),
			},
			eta: trialETA{
				limit: 10, requestsPerTrial: 8,
				maxBatchesPerSecond: 2,
			},
			want: "5s",
		},
		{
			name: "pacing supplies an estimate before throughput",
			p: tth2.TrialProgress{
				RecentTrialsPerSecond: math.NaN(),
			},
			eta: trialETA{
				limit: 10, requestsPerTrial: 2,
				maxRequestsPerSecond: 4,
			},
			want: "5s",
		},
		{
			name: "unbounded",
			p: tth2.TrialProgress{
				Trials: 20, RecentTrialsPerSecond: 1,
			},
			eta: trialETA{limit: -1},
		},
		{
			name: "complete",
			p: tth2.TrialProgress{
				Trials: 30, RecentTrialsPerSecond: 1,
			},
			eta: trialETA{limit: 30},
		},
		{
			name: "unknown rate",
			p: tth2.TrialProgress{
				Trials: 1, RecentTrialsPerSecond: math.NaN(),
			},
			eta: trialETA{limit: 30},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.eta.compact(test.p); got != test.want {
				t.Errorf("trialETA%+v.compact(%+v) = %q, want %q",
					test.eta, test.p, got, test.want)
			}
		})
	}
}

func TestCompactETADoesNotOverflowDuration(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("a 32-bit int cannot represent a duration-sized trial count")
	}
	eta := trialETA{limit: int(^uint(0) >> 1)}
	if got := eta.compact(tth2.TrialProgress{RecentTrialsPerSecond: 1e9}); got != "" {
		t.Errorf("compactETA at duration limit = %q, want no estimate", got)
	}
}

// TestDetectProgressLinePace checks the request count and both rates render
// from the report, and that observed log evidence is distinct from its
// boundary.
func TestDetectProgressLinePace(t *testing.T) {
	r := ranking.OutlierProgress{
		ProgressCore: ranking.ProgressCore{
			Cost:                 ranking.Cost{Comparisons: 100, Participations: 800},
			ComparisonsPerSecond: 640, ParticipationsPerSecond: 5120,
		},
		Detail: ranking.OutlierProgressDiagnostics{
			Threshold:  5,
			Contenders: []ranking.OutlierItemRef{{Item: 0, Late: math.Log(4)}},
		},
	}
	got := detectProgressLine(r, 200, 0, 0, ranking.Either, []string{"A"})
	if want := "100/200 comparisons | 800 request participations | 640 cmp/s, 5.1k req/s"; !strings.Contains(got, want) {
		t.Errorf("detectProgressLine = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "eta") {
		t.Errorf("detectProgressLine includes an ETA: %q", got)
	}
	if want := "log evidence 1.4/5.0"; !strings.Contains(got, want) {
		t.Errorf("detectProgressLine = %q, want it to contain %q", got, want)
	}

	got = detectProgressLine(
		r, 200, 0, 2.0/3600, ranking.Either, []string{"A"})
	if want := "0.25 cmp/h, 2.0 req/h"; !strings.Contains(got, want) {
		t.Errorf("paced detectProgressLine = %q, want it to contain %q",
			got, want)
	}
}

// TestDetectProgressLine_leaderboard checks each extreme is the true top-n for
// its own direction: from an unordered field, each board ranks by its matching
// log evidence.
func TestDetectProgressLine_leaderboard(t *testing.T) {
	names := []string{"A", "B", "C", "D"}
	r := ranking.OutlierProgress{
		Detail: ranking.OutlierProgressDiagnostics{
			Contenders: []ranking.OutlierItemRef{ // deliberately unsorted
				{Item: 2, Late: 2, Early: -1},
				{Item: 0, Late: 5, Early: -5},
				{Item: 1, Late: -4, Early: 4},
				{Item: 3, Late: 1, Early: 1},
			},
		},
	}
	got := detectProgressLine(r, -1, 0, 0, ranking.Either, names)
	if want := "late: A 5.0, C 2.0, D 1.0"; !strings.Contains(got, want) {
		t.Errorf("detectProgressLine = %q, want late board %q", got, want)
	}
	if want := "early: B 4.0, D 1.0, C -1.0"; !strings.Contains(got, want) {
		t.Errorf("detectProgressLine = %q, want early board %q", got, want)
	}
}

// TestGroupLabel checks the group heading, including the first-through-last
// span arithmetic (names[idx : idx+count]) that is easy to get off by one.
func TestGroupLabel(t *testing.T) {
	names := []string{"A", "B", "C", "D"}
	if got := groupLabel(names, 0, 1); got != "Request A" {
		t.Errorf("single = %q, want %q", got, "Request A")
	}
	if got, want := groupLabel(names, 1, 2), "Requests B through C (2 requests)"; got != want {
		t.Errorf("span = %q, want %q", got, want)
	}
	if got, want := groupLabel([]string{"p-01", "p-02", "p-03"}, 0, 3),
		"Requests p-01 through p-03 (3 requests)"; got != want {
		t.Errorf("span = %q, want %q", got, want)
	}
}

func TestSubordinateHeading(t *testing.T) {
	short := subordinateHeading("Request slow")
	if len(short) != textWidth {
		t.Fatalf("short heading width = %d, want %d: %q",
			len(short), textWidth, short)
	}
	if !strings.HasPrefix(short, "--- Request slow ") ||
		!strings.HasSuffix(short, "---") {
		t.Errorf("short heading lacks its labelled ruler: %q", short)
	}

	label := "Request " + strings.Repeat("x", textWidth)
	long := subordinateHeading(label)
	if !strings.HasPrefix(long, "--- "+label+" ") ||
		!strings.HasSuffix(long, "---") {
		t.Errorf("long heading truncated user-defined content: %q", long)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1); got != "request" {
		t.Errorf("plural(1) = %q, want request", got)
	}
	for _, n := range []int{0, 2, 10} {
		if got := plural(n); got != "requests" {
			t.Errorf("plural(%d) = %q, want requests", n, got)
		}
	}
}

// TestBodyKind covers the compact vocabulary the summary names a body with. The
// media type is taken from Content-Type, parameters and casing ignored, and
// anything unrecognised (including a missing type) is a plain "body".
func TestBodyKind(t *testing.T) {
	cases := map[string]string{
		"application/json":                  "json",
		"application/json; charset=utf-8":   "json",
		"APPLICATION/JSON":                  "json",
		"  application/json  ":              "json",
		"application/x-www-form-urlencoded": "form",
		"multipart/form-data; boundary=xyz": "multipart",
		"text/plain":                        "body",
		"application/octet-stream":          "body",
		"":                                  "body",
	}
	for ct, want := range cases {
		if got := bodyKind(ct); got != want {
			t.Errorf("bodyKind(%q) = %q, want %q", ct, got, want)
		}
	}
}

// TestByteCount covers the size vocabulary at each threshold. The boundaries
// are exact, so they are derived rather than transcribed.
func TestByteCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1<<10 - 1, "1023B"}, // last plain-bytes value
		{1 << 10, "1.0kB"},   // switches to kB exactly at a kibibyte
		{1536, "1.5kB"},      // 1.5 * 1024
		{1<<20 - 1, "1024.0kB"},
		{1 << 20, "1.0MB"}, // switches to MB exactly at a mebibyte
		{3 << 20, "3.0MB"},
	}
	for _, c := range cases {
		if got := byteCount(c.n); got != c.want {
			t.Errorf("byteCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
