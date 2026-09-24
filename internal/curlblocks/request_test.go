package curlblocks

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestNewRequest covers the request curlblocks builds from a block: body wiring
// and the default headers it implies, the Host->authority mapping, cookies
// folded into one header, and curl's URL-userinfo Basic-auth precedence. The
// body bytes and the Content-Type decision themselves are RequestBody's
// contract (tested in TestRequestBody); this asserts they land on the request.
func TestNewRequest(t *testing.T) {
	build := func(t *testing.T, b Block, url string) *http.Request {
		t.Helper()
		req, err := b.NewRequest(t.Context(), url)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if req.Body != nil {
			t.Cleanup(func() {
				if err := req.Body.Close(); err != nil {
					t.Errorf("closing built request body: %v", err)
				}
			})
		}
		return req
	}
	bodyOf := func(t *testing.T, req *http.Request) string {
		t.Helper()
		if req.GetBody == nil {
			return ""
		}
		rc, err := req.GetBody()
		if err != nil {
			t.Fatalf("GetBody: %v", err)
		}
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("closing request body: %v", err)
		}
		return string(data)
	}

	t.Run("data body is POST with a form-urlencoded Content-Type", func(t *testing.T) {
		req := build(t, Block{data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}}, "https://h/")
		if req.Method != "POST" {
			t.Errorf("method = %q, want POST", req.Method)
		}
		if got := bodyOf(t, req); got != "a=1" {
			t.Errorf("body = %q, want a=1", got)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want the form-encoding default", ct)
		}
	})
	t.Run("bodyless block has no body or Content-Type", func(t *testing.T) {
		req := build(t, Block{}, "https://h/")
		if req.Method != "GET" {
			t.Errorf("method = %q, want GET", req.Method)
		}
		if req.Body != nil {
			t.Error("bodyless request has a non-nil Body")
		}
		if ct := req.Header.Get("Content-Type"); ct != "" {
			t.Errorf("bodyless request gained Content-Type %q", ct)
		}
	})

	t.Run("Host header sets the authority, not a field line", func(t *testing.T) {
		req := build(t, Block{Headers: []Header{{Name: "Host", Value: "other.example"}}}, "https://h/")
		if req.Host != "other.example" {
			t.Errorf("req.Host = %q, want other.example", req.Host)
		}
		if got := req.Header.Get("Host"); got != "" {
			t.Errorf("Host also set as a field line: %q", got)
		}
	})

	// The Accept a body kind implies beats the baked "*/*" baseline (which is
	// curlblocks' text, not the user's) but yields to an explicit -H; a
	// suppression removes both. Each case parses a command line, so the baseline
	// is really in play, and asserts the one header value that reaches the wire.
	t.Run("Accept precedence: body default over baseline, -H over both", func(t *testing.T) {
		tests := []struct {
			name string
			args []string
			want string // "" means no Accept header at all
		}{
			{"baseline alone", []string{"https://h/"}, acceptAll},
			{"a data body keeps the baseline", []string{"https://h/", "-d", "a=1"}, acceptAll},
			{"--json replaces the baseline", []string{"https://h/", "--json", `{"a":1}`}, "application/json"},
			{"-H beats the baseline", []string{"https://h/", "-H", "Accept: text/plain"}, "text/plain"},
			{
				name: "-H beats a body default too",
				args: []string{"https://h/", "--json", `{"a":1}`, "-H", "Accept: text/plain"},
				want: "text/plain",
			},
			{"'-H Accept:' removes the baseline", []string{"https://h/", "-H", "Accept:"}, ""},
			{
				name: "'-H Accept:' removes a body default too",
				args: []string{"https://h/", "--json", `{"a":1}`, "-H", "Accept:"},
				want: "",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				plan, err := New().Parse(tt.args)
				if err != nil {
					t.Fatalf("New().Parse(%q): %v", tt.args, err)
				}
				b := plan.Blocks[0]
				req := build(t, b, b.URLs[0])
				if got := req.Header.Values("Accept"); len(got) > 1 {
					t.Fatalf("Accept sent %d times (%v), want at most one", len(got), got)
				}
				if got := req.Header.Get("Accept"); got != tt.want {
					t.Errorf("Accept = %q, want %q", got, tt.want)
				}
			})
		}
	})

	t.Run("cookies fold into one Cookie header", func(t *testing.T) {
		b := Block{Cookies: []Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}}
		req := build(t, b, "https://h/")
		if got := req.Header.Get("Cookie"); got != "a=1; b=2" {
			t.Errorf("Cookie = %q, want %q", got, "a=1; b=2")
		}
		bare := build(t, Block{}, "https://h/")
		if _, ok := bare.Header["Cookie"]; ok {
			t.Errorf("bare request gained a Cookie header: %q", bare.Header.Get("Cookie"))
		}
	})

	// The folded -b header is a default, so a hand-written Cookie header replaces
	// it outright rather than joining it on a second line — curl's rule, and the
	// only one that keeps a single Cookie field line on the wire.
	t.Run("an explicit Cookie header replaces the folded cookies", func(t *testing.T) {
		req := build(t, Block{
			Headers: []Header{{Name: "Cookie", Value: "hand=written"}},
			Cookies: []Cookie{{Name: "a", Value: "1"}},
		}, "https://h/")
		want := []string{"hand=written"}
		if got := req.Header.Values("Cookie"); !slices.Equal(got, want) {
			t.Errorf("Cookie lines = %v, want %v", got, want)
		}
	})

	t.Run("'-H Cookie:' suppresses the folded cookies", func(t *testing.T) {
		req := build(t, Block{
			SuppressedHeaders: []string{"Cookie"},
			Cookies:           []Cookie{{Name: "a", Value: "1"}},
		}, "https://h/")
		if got, ok := req.Header["Cookie"]; ok {
			t.Errorf("Cookie = %v, want no Cookie header", got)
		}
	})

	// --form-escape reaches the body through the block, not through a parameter.
	// URL userinfo is the lowest-precedence Basic-auth source (below -u and an
	// explicit -H). A bare "user@" means an empty password (curl-compatible
	// base64); a pre-set Authorization wins; a suppressed one drops it.
	t.Run("URL userinfo becomes lowest-precedence Basic auth", func(t *testing.T) {
		auth := func(t *testing.T, url string, b Block) string {
			return build(t, b, url).Header.Get("Authorization")
		}
		if got := auth(t, "https://user:pass@h/", Block{}); got != "Basic dXNlcjpwYXNz" {
			t.Errorf("user:pass -> %q, want Basic base64(user:pass)", got)
		}
		if got := auth(t, "https://user@h/", Block{}); got != "Basic dXNlcjo=" {
			t.Errorf("bare user -> %q, want Basic base64(user:)", got)
		}
		blockAuth := Block{Headers: []Header{{Name: "Authorization", Value: "Bearer keep"}}}
		if got := auth(t, "https://user:pass@h/", blockAuth); got != "Bearer keep" {
			t.Errorf("URL userinfo overrode block auth: %q, want Bearer keep", got)
		}
		suppressed := Block{SuppressedHeaders: []string{"Authorization"}}
		if got := auth(t, "https://user:pass@h/", suppressed); got != "" {
			t.Errorf("suppressed Authorization still set from URL: %q", got)
		}
		if got := auth(t, "https://h/", Block{}); got != "" {
			t.Errorf("no userinfo yielded Authorization %q, want none", got)
		}
	})
}

func TestNewRequestWithMultipartBoundary(t *testing.T) {
	const boundary = "0123456789abcdef"
	b := Block{
		form: []formPart{
			{Name: "field", Kind: formLiteral, Value: "value"},
			{
				Name: "upload", Kind: formFile, Value: "document.bin",
				FileName: "named.bin", Type: "application/x-test",
			},
		},
		readFile: fakeFS(map[string]string{"document.bin": "contents"}),
	}
	req, err := b.NewRequestWithMultipartBoundary(
		t.Context(), "https://h/", boundary)
	if err != nil {
		t.Fatalf("NewRequestWithMultipartBoundary: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" ||
		params["boundary"] != boundary {
		t.Fatalf("Content-Type = %q: media type %q, params %v, error %v",
			req.Header.Get("Content-Type"), mediaType, params, err)
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	mr := multipart.NewReader(rc, boundary)
	field, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	fieldValue, err := io.ReadAll(field)
	if err != nil || field.FormName() != "field" || field.FileName() != "" ||
		string(fieldValue) != "value" {
		t.Errorf("field = name %q, filename %q, value %q, error %v",
			field.FormName(), field.FileName(), fieldValue, err)
	}
	upload, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	uploadValue, err := io.ReadAll(upload)
	if err != nil || upload.FormName() != "upload" ||
		upload.FileName() != "named.bin" ||
		upload.Header.Get("Content-Type") != "application/x-test" ||
		string(uploadValue) != "contents" {
		t.Errorf("upload = name %q, filename %q, type %q, value %q, error %v",
			upload.FormName(), upload.FileName(),
			upload.Header.Get("Content-Type"), uploadValue, err)
	}
	if _, err := mr.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("multipart tail error = %v, want EOF", err)
	}

	_, err = b.NewRequestWithMultipartBoundary(
		t.Context(), "https://h/", "invalid\nboundary")
	if err == nil || !strings.Contains(err.Error(), "invalid multipart boundary") {
		t.Errorf("invalid boundary error = %v", err)
	}
}

func TestNewRequestWithMultipartBoundaryPreservesOtherBodiesAndHeaders(t *testing.T) {
	const boundary = "fixed-boundary"
	tests := []struct {
		name        string
		block       Block
		wantBody    string
		wantType    string
		wantPresent bool
	}{
		{
			name: "data", block: Block{data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}},
			wantBody: "a=1", wantType: "application/x-www-form-urlencoded",
			wantPresent: true,
		},
		{
			name: "JSON", block: Block{json: []jsonPiece{{Spec: `{"a":1}`}}},
			wantBody: `{"a":1}`, wantType: "application/json", wantPresent: true,
		},
		{name: "bodyless", block: Block{}},
		{
			name: "authored multipart type",
			block: Block{
				form:    []formPart{{Name: "a", Kind: formLiteral, Value: "1"}},
				Headers: []Header{{Name: "Content-Type", Value: "custom/type; x=Exact"}},
			},
			wantType: "custom/type; x=Exact", wantPresent: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := test.block.NewRequestWithMultipartBoundary(
				t.Context(), "https://h/", boundary)
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Content-Type"); got != test.wantType {
				t.Errorf("Content-Type = %q, want %q", got, test.wantType)
			}
			if (req.GetBody != nil) != test.wantPresent {
				t.Fatalf("GetBody presence = %t, want %t",
					req.GetBody != nil, test.wantPresent)
			}
			if test.wantBody != "" {
				rc, err := req.GetBody()
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(rc)
				closeErr := rc.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("read body: %v; close body: %v", readErr, closeErr)
				}
				if string(got) != test.wantBody {
					t.Errorf("body = %q, want %q", got, test.wantBody)
				}
			}
		})
	}
}

// TestNewRequestErrors checks NewRequest's failure modes: a body it cannot
// materialise, and a target or method net/http will not accept.
func TestNewRequestErrors(t *testing.T) {
	t.Run("an unreadable @file body propagates", func(t *testing.T) {
		b := Block{data: []dataPiece{{Kind: dataBinary, Spec: "@nope.txt"}}, readFile: noFiles}
		_, err := b.NewRequest(t.Context(), "https://h/")
		if err == nil || !strings.Contains(err.Error(), "nope.txt") {
			t.Fatalf("err = %v, want one naming nope.txt", err)
		}
	})
	t.Run("an invalid URL propagates", func(t *testing.T) {
		if _, err := (Block{}).NewRequest(t.Context(), "://"); err == nil {
			t.Fatal("NewRequest with a schemeless \"://\" target: want an error")
		}
	})
	t.Run("an invalid method propagates", func(t *testing.T) {
		if _, err := (Block{Method: "BAD METHOD"}).NewRequest(t.Context(), "https://h/"); err == nil {
			t.Fatal("NewRequest with a space in the method: want an error")
		}
	})
}

// TestHTTPMethod checks method resolution: -X is verbatim (curl does not change
// its case), a body (-d, -F, or --json) defaults to POST, and a bodyless block
// to GET.
func TestHTTPMethod(t *testing.T) {
	tests := []struct {
		name string
		b    Block
		want string
	}{
		{name: "no body is GET", b: Block{}, want: "GET"},
		{name: "data is POST", b: Block{data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}}, want: "POST"},
		{name: "form is POST", b: Block{form: []formPart{{Name: "f", Kind: formLiteral, Value: "v"}}}, want: "POST"},
		{name: "json is POST", b: Block{json: []jsonPiece{{Spec: "{}"}}}, want: "POST"},
		{
			name: "explicit -X wins over body default",
			b:    Block{Method: "PATCH", data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}},
			want: "PATCH",
		},
		{name: "explicit -X kept verbatim, not upper-cased", b: Block{Method: "get"}, want: "get"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.HTTPMethod(); got != tt.want {
				t.Errorf("HTTPMethod() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRequestBody checks the unified materialiser's dispatch and default-header
// decision (the body bytes themselves are covered by
// TestBlockBody/TestFormBody/TestJSONBody): data yields the form-urlencoded
// type, a form yields multipart with a boundary, --json yields application/json
// Content-Type and Accept, a bodyless block yields nothing, and a user-set or
// suppressed header is dropped from the defaults.
func TestRequestBody(t *testing.T) {
	t.Run("data defaults to form-urlencoded", func(t *testing.T) {
		body, present, hs, err := (Block{data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}}).RequestBody()
		ct, ok := lookupHeader(hs, "Content-Type")
		if err != nil || !present || string(body) != "a=1" || !ok || ct != "application/x-www-form-urlencoded" {
			t.Errorf("got body=%q present=%v headers=%v err=%v", body, present, hs, err)
		}
	})
	// The boundary is chosen at random, so the truth to assert is that the one
	// announced in the Content-Type is the one delimiting the body.
	t.Run("form yields multipart with a boundary", func(t *testing.T) {
		body, present, hs, err := (Block{
			form: []formPart{{Name: "f", Kind: formLiteral, Value: "v"}},
		}).RequestBody()
		ct, ok := lookupHeader(hs, "Content-Type")
		if err != nil || !present || !ok {
			t.Fatalf("got present=%v headers=%v err=%v", present, hs, err)
		}
		boundary, found := strings.CutPrefix(ct, "multipart/form-data; boundary=")
		if !found || boundary == "" {
			t.Fatalf("Content-Type = %q, want multipart/form-data with a boundary", ct)
		}
		if !strings.Contains(string(body), "--"+boundary+"\r\n") {
			t.Errorf("body %q does not open with the announced boundary %q", body, boundary)
		}
	})
	t.Run("json yields application/json Content-Type and Accept", func(t *testing.T) {
		body, present, hs, err := (Block{json: []jsonPiece{{Spec: `{"a":1}`}}}).RequestBody()
		ct, ctOK := lookupHeader(hs, "Content-Type")
		ac, acOK := lookupHeader(hs, "Accept")
		if err != nil || !present || string(body) != `{"a":1}` ||
			!ctOK || ct != "application/json" ||
			!acOK || ac != "application/json" {
			t.Errorf("got body=%q present=%v headers=%v err=%v", body, present, hs, err)
		}
	})
	t.Run("no body yields nothing", func(t *testing.T) {
		body, present, hs, err := (Block{}).RequestBody()
		if err != nil || present || body != nil || len(hs) != 0 {
			t.Errorf("got body=%q present=%v headers=%v err=%v", body, present, hs, err)
		}
	})
	t.Run("user Content-Type header drops the default", func(t *testing.T) {
		b := Block{
			data:    []dataPiece{{Kind: dataASCII, Spec: "a=1"}},
			Headers: []Header{{Name: "Content-Type", Value: "application/json"}},
		}
		_, present, hs, err := b.RequestBody()
		if _, ok := lookupHeader(hs, "Content-Type"); err != nil || !present || ok {
			t.Errorf("got present=%v headers=%v err=%v, want no default Content-Type", present, hs, err)
		}
	})
	t.Run("suppressed Content-Type drops the default", func(t *testing.T) {
		b := Block{data: []dataPiece{{Kind: dataASCII, Spec: "a=1"}}, SuppressedHeaders: []string{"Content-Type"}}
		_, present, hs, err := b.RequestBody()
		if _, ok := lookupHeader(hs, "Content-Type"); err != nil || !present || ok {
			t.Errorf("got present=%v headers=%v err=%v, want no default Content-Type", present, hs, err)
		}
	})
	t.Run("json Accept can be suppressed while Content-Type stays", func(t *testing.T) {
		b := Block{json: []jsonPiece{{Spec: "{}"}}, SuppressedHeaders: []string{"Accept"}}
		_, _, hs, err := b.RequestBody()
		_, ctOK := lookupHeader(hs, "Content-Type")
		_, acOK := lookupHeader(hs, "Accept")
		if err != nil || !ctOK || acOK {
			t.Errorf("got headers=%v err=%v, want Content-Type kept and Accept dropped", hs, err)
		}
	})
}

// TestBlankMethodRejected checks that a blank -X is refused rather than read as
// "no method given": HTTPMethod treats an unset method as "let the body
// decide", so accepting a blank one would turn -X into a silent GET on a
// bodyless block. A block overriding the preamble's blank is not affected,
// since the value the request would use is the one checked.
func TestBlankMethodRejected(t *testing.T) {
	for _, args := range [][]string{
		{"https://h/", "-X", ""},
		{"-X", "", "--block", "https://h/"},
	} {
		if _, err := New().Parse(args); err == nil || !strings.Contains(err.Error(), "-X/--request") {
			t.Errorf("Parse(%q) error = %v, want one naming -X", args, err)
		}
	}

	plan, err := New().Parse([]string{"-X", "", "--block", "https://h/", "-X", "PUT"})
	if err != nil {
		t.Fatalf("a block's own method replaces the blank baseline: %v", err)
	}
	if got := plan.Blocks[0].HTTPMethod(); got != "PUT" {
		t.Errorf("method = %q, want PUT", got)
	}
}

// TestDefaultHeaderNames checks the report of which headers curlblocks supplied
// itself: the baked baseline, the names a body kind implies, and neither where
// the user set or suppressed the name. A -b cookie is the user's content, not a
// default.
func TestDefaultHeaderNames(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ua   string
		want []string
	}{
		{
			name: "the baked baseline alone",
			args: []string{"https://a"},
			want: []string{"Accept"},
		},
		{
			name: "a configured User-Agent joins it",
			args: []string{"https://a"},
			ua:   "tool/1",
			want: []string{"Accept", "User-Agent"},
		},
		{
			name: "a data body implies Content-Type",
			args: []string{"-d", "x=1", "https://a"},
			want: []string{"Accept", "Content-Type"},
		},
		{
			name: "--json implies Content-Type, and Accept stays one entry",
			args: []string{"--json", `{"a":1}`, "https://a"},
			want: []string{"Accept", "Content-Type"},
		},
		{
			name: "a header the user set is theirs, not a default",
			args: []string{"-H", "Accept: text/plain", "-d", "x=1", "https://a"},
			want: []string{"Content-Type"},
		},
		{
			name: "a suppressed implied header is reported by neither name",
			args: []string{"-H", "Content-Type:", "-H", "Accept:", "-d", "x=1", "https://a"},
			want: nil,
		},
		{
			name: "a -b cookie is the user's content",
			args: []string{"-b", "s=1", "https://a"},
			want: []string{"Accept"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []ParserOption
			if tt.ua != "" {
				opts = append(opts, WithUserAgent(tt.ua))
			}
			plan, err := New(opts...).Parse(tt.args)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			got := plan.Blocks[0].DefaultHeaderNames()
			sort.Strings(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DefaultHeaderNames() = %v, want %v", got, tt.want)
			}
		})
	}
}
