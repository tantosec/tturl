package curlblocks

import (
	"context"
	"strings"
	"testing"
)

func TestProtocolScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    []Protocol
		invalid bool
	}{
		{name: "default", args: []string{"https://example.test"}, want: []Protocol{HTTP2}},
		{
			name: "inherited override",
			args: []string{"--http1.1", "--block", "https://example.test/a", "--http2", "--block", "https://example.test/b"},
			want: []Protocol{HTTP2, HTTP11},
		},
		{
			name: "neighbour isolation",
			args: []string{"--block", "https://example.test/a", "--http1.1", "--block", "https://example.test/b"},
			want: []Protocol{HTTP11, HTTP2},
		},
		{
			name:    "preamble conflict",
			args:    []string{"--http1.1", "--http2", "--block", "https://example.test", "--http2"},
			invalid: true,
		},
		{name: "block conflict", args: []string{"--block", "https://example.test", "--http1.1", "--http2"}, invalid: true},
		{name: "disabled selection", args: []string{"--http1.1=false", "https://example.test"}, want: []Protocol{HTTP2}},
		{
			name: "repeated switch",
			args: []string{"--http1.1", "--http1.1=false", "--http2", "https://example.test"},
			want: []Protocol{HTTP2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New()
			versions := p.ProtocolFlags(p.Shared)
			plan, err := p.Parse(tc.args)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "cannot both") {
					t.Fatalf("conflict error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Blocks) != len(tc.want) {
				t.Fatalf("blocks: %d", len(plan.Blocks))
			}
			for i, b := range plan.Blocks {
				if got := versions.Get(b); got != tc.want[i] {
					t.Errorf("block %d: got %v, want %v", i, got, tc.want[i])
				}
			}
		})
	}
}

func TestProtocolMaterialisationAndExpansion(t *testing.T) {
	p := New()
	versions := p.ProtocolFlags(p.Shared)
	plan, err := p.Parse([]string{
		"--http1.1", "--block", "http://example.test/V", "--vary", "V={a,b}",
		"-H", "Transfer-Encoding: chunked",
		"-H", "Connection: close",
	})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if versions.Get(group.Block) != HTTP11 {
			t.Fatal("expanded protocol missing")
		}
		req, err := group.Block.NewRequest(context.Background(), group.URL)
		if err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Transfer-Encoding") != "chunked" || req.Header.Get("Connection") != "close" {
			t.Fatal("materialisation removed protocol fields")
		}
	}
	if _, err := New().Parse([]string{"--http1.1", "https://example.test"}); err == nil {
		t.Fatal("protocol grammar must require opt-in")
	}
}
