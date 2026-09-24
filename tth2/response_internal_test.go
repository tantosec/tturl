package tth2

import (
	"testing"

	"golang.org/x/net/http2/hpack"
)

func TestProjectedResponseContentLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []string
		want   int64
	}{
		{"absent", nil, -1},
		{"zero", []string{"0"}, 0},
		{"declared", []string{"24"}, 24},
		{"matching duplicates", []string{"24", "24"}, 24},
		{"matching list", []string{"24, 24"}, 24},
		{"conflicting", []string{"24", "25"}, -1},
		{"malformed", []string{"nope"}, -1},
		{"negative", []string{"-1"}, -1},
	}
	for _, tc := range tests {
		for _, capture := range []bool{false, true} {
			name := tc.name + "/metadata"
			if capture {
				name = tc.name + "/capture"
			}
			t.Run(name, func(t *testing.T) {
				fields := []hpack.HeaderField{{Name: ":status", Value: "200"}}
				for _, value := range tc.values {
					fields = append(fields,
						hpack.HeaderField{Name: "content-length", Value: value})
				}
				kind, resp, trailer := projectResponseHeaderBlock(fields, capture)
				if kind != responseHeaderFinal || resp == nil || trailer != nil {
					t.Fatalf("projection = %v/%+v/%v", kind, resp, trailer)
				}
				if resp.ContentLength != tc.want {
					t.Errorf("ContentLength = %d, want %d",
						resp.ContentLength, tc.want)
				}
				if (resp.Header != nil) != capture {
					t.Errorf("Header = %v with capture %t", resp.Header, capture)
				}
			})
		}
	}
}

func TestProjectResponseHeaderBlockRetention(t *testing.T) {
	t.Parallel()
	fields := []hpack.HeaderField{
		{Name: ":status", Value: "201"},
		{Name: "x-evidence", Value: "accepted"},
	}
	for _, capture := range []bool{false, true} {
		kind, response, trailer := projectResponseHeaderBlock(fields, capture)
		if kind != responseHeaderFinal || response == nil || trailer != nil {
			t.Fatalf("final projection = %v/%+v/%v", kind, response, trailer)
		}
		if response.Status != "201 Created" || response.StatusCode != 201 {
			t.Errorf("status = %q/%d", response.Status, response.StatusCode)
		}
		if capture {
			if got := response.Header.Get("X-Evidence"); got != "accepted" {
				t.Errorf("X-Evidence = %q, want accepted", got)
			}
		} else if response.Header != nil {
			t.Errorf("metadata projection retained Header = %v", response.Header)
		}
	}

	for _, capture := range []bool{false, true} {
		kind, response, trailer := projectResponseHeaderBlock(
			[]hpack.HeaderField{{Name: "x-trailer", Value: "accepted"}},
			capture)
		if kind != responseHeaderTrailer || response != nil {
			t.Fatalf("trailer projection = %v/%+v/%v", kind, response, trailer)
		}
		if capture {
			if got := trailer.Get("X-Trailer"); got != "accepted" {
				t.Errorf("X-Trailer = %q, want accepted", got)
			}
		} else if trailer != nil {
			t.Errorf("metadata projection retained Trailer = %v", trailer)
		}
	}

	kind, response, trailer := projectResponseHeaderBlock([]hpack.HeaderField{
		{Name: ":status", Value: "103"},
		{Name: "link", Value: "</style.css>; rel=preload"},
	}, true)
	if kind != responseHeaderInformational || response != nil || trailer != nil {
		t.Errorf("informational projection = %v/%+v/%v", kind, response, trailer)
	}
}
