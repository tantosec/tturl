package main

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

func TestPreparedResolveBodyRelease(t *testing.T) {
	t.Parallel()
	request := func(body string) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(
			t.Context(), http.MethodPost, "https://example.com/", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	tests := []struct {
		name       string
		prepared   prepared
		padding    []tth2.RequestPadding
		wantBodies bool
	}{
		{
			name: "empty body",
			prepared: prepared{
				materialised: &materialised{reqs: []*http.Request{request("")}},
			},
		},
		{
			name: "measured body",
			prepared: prepared{
				materialised: &materialised{
					reqs: []*http.Request{request("measured")},
				},
			},
			wantBodies: true,
		},
		{
			name: "distinct warmup body",
			prepared: prepared{
				materialised: &materialised{
					reqs:       []*http.Request{request("")},
					warmupReqs: []*http.Request{request("warmup")},
				},
			},
			wantBodies: true,
		},
		{
			name: "position body padding",
			prepared: prepared{
				materialised: &materialised{reqs: []*http.Request{request("")}},
			},
			padding:    []tth2.RequestPadding{{BodyParams: 1}},
			wantBodies: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := test.prepared.resolveBodyRelease(
				bodyReleasePlan{}, test.padding)
			if got.hasBodyBytes != test.wantBodies {
				t.Errorf("hasBodyBytes = %t, want %t",
					got.hasBodyBytes, test.wantBodies)
			}
		})
	}
}

// assembleArgs parses args under a command's flag set and assembles the plan,
// as prepare does minus the network. The command only picks the flag set;
// the request grammar is the same under all three.
func assembleArgs(t *testing.T, args ...string) *assembled {
	t.Helper()
	layout := newCommand(commandByID(commandRace))
	p := layout.parser
	addSharedFlags(layout)
	plan, err := p.Parse(args)
	if err != nil {
		t.Fatalf("parse(%q): %v", args, err)
	}
	a, err := assemble(t.Context(), plan)
	if err != nil {
		t.Fatalf("assemble(%q): %v", args, err)
	}
	return a
}

// TestAssemble covers the network-free orchestration prepare delegates to: the
// block->request fan-out, the NameSpec-in-lockstep name resolution, and the
// https default for schemeless URLs. A malformed request or colliding --name
// must surface here, before any connection is opened.
func TestAssemble(t *testing.T) {
	a := assembleArgs(t,
		"--block", "example.com/a", "--name", "alpha",
		"--block", "example.com/b", "--repeat", "2", "--name", "beta")
	groups, names := a.groups, a.labels

	if len(groups) != 2 || groups[0].count != 1 || groups[1].count != 2 {
		t.Fatalf("groups = %d entries with counts %d,%d; want 2 with 1,2",
			len(groups), groups[0].count, groups[1].count)
	}
	// beta repeats, so it is suffixed; the width follows the count (two ->
	// one digit).
	if want := []string{"alpha", "beta~1", "beta~2"}; !slices.Equal(names, want) {
		t.Errorf("labels = %v, want %v", names, want)
	}
	// Schemeless URLs default to https, and the request carries the block's path.
	if got := groups[0].req.URL.Scheme; got != "https" {
		t.Errorf("scheme = %q, want https (defaulted)", got)
	}
	if got := groups[1].req.URL.Path; got != "/b" {
		t.Errorf("path = %q, want /b", got)
	}
}

func TestAssembleWarmupRequestsFollowMeasuredIDs(t *testing.T) {
	p := newCommand(commandByID(commandRace)).parser
	warmupOnly := p.Block.Bool("warmup-only", "", false, "test")
	plan, err := p.Parse([]string{
		"--block", "example.com/warm", "--name", "warm", "--warmup-only",
		"--block", "example.com/a", "--name", "A",
		"--block", "example.com/b", "--name", "B", "--repeat", "2",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, err := assembleWithRoles(t.Context(), plan, func(blockIndex int) bool {
		return warmupOnly.Get(plan.Blocks[blockIndex])
	})
	if err != nil {
		t.Fatalf("assembleWithRoles: %v", err)
	}
	if want := []string{"A", "B~1", "B~2", "warm"}; !slices.Equal(a.labels, want) {
		t.Errorf("labels = %v, want measured IDs followed by warmup IDs %v",
			a.labels, want)
	}
	measured := expandRole(a.groups, false)
	warmup := expandRole(a.groups, true)
	if len(measured) != 3 || len(warmup) != 1 {
		t.Fatalf("measured/warmup widths = %d/%d, want 3/1",
			len(measured), len(warmup))
	}
	if measured[0].URL.Path != "/a" || warmup[0].URL.Path != "/warm" {
		t.Errorf("measured/warmup paths = %q/%q, want /a and /warm",
			measured[0].URL.Path, warmup[0].URL.Path)
	}
	var summary bytes.Buffer
	summarise(&summary, a.groups, fullRunReferences(a.labels), false)
	for _, want := range []string{
		"/warm  warmup", "3 trial requests, 1 warmup request",
	} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, summary.String())
		}
	}
}

func TestAssembleReusesMultipartBoundaryAcrossExperiment(t *testing.T) {
	layout := newCommand(commandByID(commandRace))
	warmupOnly := layout.parser.Block.Bool("warmup-only", "", false, "test")
	plan, err := layout.parser.Parse([]string{
		"--block", "https://h/measured", "--form-string", "kind=measured",
		"--name", "measured", "--repeat", "2",
		"--block", "https://h/fan/VALUE", "--form-string", "kind=VALUE",
		"--name", "fan", "--vary", "VALUE={one,two}",
		"--block", "https://h/prime", "--form-string", "kind=prime",
		"--name", "prime", "--warmup-only",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	source := bytes.NewReader(bytes.Repeat([]byte{0xab}, multipartBoundaryBytes))
	a, err := assembleWithRolesAndBoundarySource(
		t.Context(), plan, func(blockIndex int) bool {
			return warmupOnly.Get(plan.Blocks[blockIndex])
		}, source)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	wantLabels := []string{
		"measured~1", "measured~2", "fan:VALUE=one", "fan:VALUE=two", "prime",
	}
	if !slices.Equal(a.labels, wantLabels) {
		t.Errorf("labels = %v, want %v", a.labels, wantLabels)
	}
	wantBoundary := strings.Repeat("ab", multipartBoundaryBytes)
	wantValues := []string{"measured", "measured", "one", "two", "prime"}
	requests := expand(a.groups)
	if len(requests) != len(wantValues) {
		t.Fatalf("requests = %d, want %d", len(requests), len(wantValues))
	}
	for i, req := range requests {
		boundary, values := readMultipartRequest(t, req)
		if boundary != wantBoundary {
			t.Errorf("request %d boundary = %q, want %q",
				i, boundary, wantBoundary)
		}
		if want := map[string]string{"kind": wantValues[i]}; !mapsEqual(values, want) {
			t.Errorf("request %d fields = %v, want %v", i, values, want)
		}
	}
	if !a.groups[len(a.groups)-1].warmup {
		t.Error("priming request lost its warmup role")
	}
}

func TestAssembleChoosesBoundaryFromEachSource(t *testing.T) {
	plan, err := curlblocks.New().Parse([]string{
		"https://h/", "--form-string", "field=value",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	boundaries := make([]string, 2)
	for i, value := range []byte{0x11, 0x22} {
		a, err := assembleWithRolesAndBoundarySource(
			t.Context(), plan, nil,
			bytes.NewReader(bytes.Repeat([]byte{value}, multipartBoundaryBytes)))
		if err != nil {
			t.Fatalf("assemble %d: %v", i, err)
		}
		boundaries[i], _ = readMultipartRequest(t, a.groups[0].req)
	}
	if boundaries[0] == boundaries[1] {
		t.Errorf("separate assemblies used the same boundary %q", boundaries[0])
	}
}

func TestAssembleReturnsBoundarySourceFailure(t *testing.T) {
	plan, err := curlblocks.New().Parse([]string{
		"https://h/", "--form-string", "field=value",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := errors.New("random source failed")
	assembled, err := assembleWithRolesAndBoundarySource(
		t.Context(), plan, nil, boundarySourceErrorReader{err: want})
	if assembled != nil || !errors.Is(err, want) ||
		!strings.Contains(err.Error(), "generate multipart boundary") {
		t.Fatalf("assemble = %+v, %v; want wrapped source failure", assembled, err)
	}
}

type boundarySourceErrorReader struct{ err error }

func (r boundarySourceErrorReader) Read([]byte) (int, error) { return 0, r.err }

func readMultipartRequest(
	t *testing.T,
	req *http.Request,
) (string, map[string]string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q: media type %q, params %v, error %v",
			req.Header.Get("Content-Type"), mediaType, params, err)
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	fields := make(map[string]string)
	mr := multipart.NewReader(rc, params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		fields[part.FormName()] = string(value)
	}
	return params["boundary"], fields
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// TestAssembleOriginFromSubstitutedURLs checks the origin pre-flight reads the
// built requests, not the plan's templates. A --vary token is not necessarily a
// valid URL in place. A '%' in its name is not a valid escape, so parsing a
// template would reject a command whose every substituted URL is fine.
func TestAssembleOriginFromSubstitutedURLs(t *testing.T) {
	a := assembleArgs(t, "h:8443/item/P%X", "--vary", "P%X={1,2}")
	origin, err := tth2.BatchOrigin(expand(a.groups))
	if err != nil {
		t.Fatalf("BatchOrigin: %v", err)
	}
	if origin != "h:8443" {
		t.Errorf("origin = %q, want h:8443", origin)
	}
	if got := []string{a.groups[0].req.URL.Path, a.groups[1].req.URL.Path}; !slices.Equal(
		got, []string{"/item/1", "/item/2"}) {
		t.Errorf("paths = %v, want the substituted /item/1 and /item/2", got)
	}
}

// TestAssembleBlockError verifies explicit block attribution and the absent
// prefix for the implicit block created by a bare preamble URL.
func TestAssembleBlockError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	assembleErr := func(t *testing.T, args ...string) error {
		t.Helper()
		layout := newCommand(commandByID(commandRace))
		p := layout.parser
		addSharedFlags(layout)
		plan, err := p.Parse(args)
		if err != nil {
			t.Fatalf("parse(%q): %v", args, err)
		}
		_, err = assemble(t.Context(), plan)
		if err == nil {
			t.Fatalf("assemble(%q) succeeded; want an error from the unreadable body file", args)
		}
		return err
	}

	t.Run("the failing block is named by its 1-based number", func(t *testing.T) {
		err := assembleErr(t, "--block", "https://h/a", "--block", "https://h/b", "--data", "@"+missing)
		if !strings.HasPrefix(err.Error(), "block 2: ") {
			t.Errorf("error = %v, want it prefixed 'block 2: '", err)
		}
	})

	t.Run("a bare preamble URL opens no block, so nothing is numbered", func(t *testing.T) {
		err := assembleErr(t, "https://h/a", "--data", "@"+missing)
		if strings.Contains(err.Error(), "block ") {
			t.Errorf("error = %v, want no block prefix for the implicit block", err)
		}
	})
}

// TestAssembleFan covers a --vary block: it fans into one request per value,
// each labelled Name:TOKEN=value, and every variant group shares one fanInfo.
// The token substitutes into each request.
func TestAssembleFan(t *testing.T) {
	a := assembleArgs(t, "--block", "example.com/item/FUZZ", "--name", "probe", "--vary", "FUZZ=0-2")
	groups, names := a.groups, a.labels
	if want := []string{"probe:FUZZ=0", "probe:FUZZ=1", "probe:FUZZ=2"}; !slices.Equal(names, want) {
		t.Errorf("labels = %v, want %v", names, want)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	fi := groups[0].fan
	if fi == nil {
		t.Fatal("a fan variant carries no fanInfo")
	}
	for i, g := range groups {
		if g.fan != fi {
			t.Errorf("group %d does not share the fan's fanInfo", i)
		}
	}
	if fi.name != "probe" || len(fi.tokens) != 1 || fi.tokens[0] != "FUZZ" || fi.source != "0..2" {
		t.Errorf("fanInfo = {name:%q tokens:%v source:%q}, want {probe [FUZZ] 0..2}", fi.name, fi.tokens, fi.source)
	}
	if got := groups[2].req.URL.Path; got != "/item/2" {
		t.Errorf("variant path = %q, want /item/2 (token substituted)", got)
	}
}

// TestAssembleFanRepeat covers --vary composed with --repeat: each value is
// sent --repeat times, so the shared value's label takes the numeric suffix
// ResolveNames applies to any repeated name.
func TestAssembleFanRepeat(t *testing.T) {
	a := assembleArgs(t, "--block", "example.com/FUZZ", "--vary", "FUZZ={0,50}", "--repeat", "2")
	if want := []string{"FUZZ=0~1", "FUZZ=0~2", "FUZZ=50~1", "FUZZ=50~2"}; !slices.Equal(a.labels, want) {
		t.Errorf("labels = %v, want %v", a.labels, want)
	}
}

func TestAssembleFanLabelsRemainComplete(t *testing.T) {
	aa, bb := strings.Repeat("a", 20), strings.Repeat("b", 20)
	a := assembleArgs(t, "--block", "https://h/VAL", "--vary", "VAL={"+aa+","+bb+"}")
	if want := []string{"VAL=" + aa, "VAL=" + bb}; !slices.Equal(a.labels, want) {
		t.Errorf("labels = %v, want %v", a.labels, want)
	}
	if a.pins["VAL"] == nil {
		t.Error("fan should be pinnable by its token")
	}
}

// TestAssembleRaggedNote covers the note a pitchfork fan carries when its
// sources differ in length: curlblocks truncates to the shortest, and the tool
// says so. The note is keyed by the fan rather than by the variant, so a fan of
// many requests still explains itself exactly once.
func TestAssembleRaggedNote(t *testing.T) {
	a := assembleArgs(t, "https://h/a?x=A&y=B",
		"--vary", "A={1,2,3}", "--vary", "B={x,y}", "--vary-mode", "pitchfork")

	if len(a.notes) != 1 {
		t.Fatalf("notes = %v, want exactly one note for the one ragged fan", a.notes)
	}
	// It has to name the fan it is about and the length actually used, or a reader
	// cannot tell which sweep was cut short or by how much.
	for _, want := range []string{"A,B", "truncated to the shortest (2)"} {
		if !strings.Contains(a.notes[0], want) {
			t.Errorf("note = %q, want it to mention %q", a.notes[0], want)
		}
	}
	if len(a.labels) != 2 {
		t.Errorf("names = %v, want the two paired requests the note describes", a.labels)
	}
}
