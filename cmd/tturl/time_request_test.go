package main

import (
	"context"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/curlblocks"
)

func TestTimeRequestProtocolMaterialisation(t *testing.T) {
	parser := curlblocks.New(curlblocks.WithDefaultScheme("https"))
	protocols := parser.ProtocolFlags(parser.Shared)
	roles := addRequestRoleFlags(parser)
	plan, err := parser.Parse([]string{
		"--http1.1", "--block", "https://one.test/a", "--repeat", "2",
		"-H", "Connection: keep-alive", "--block", "https://one.test/b", "--http2", "-H", "Content-Length: 9",
	})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := materialiseTimeRequests(context.Background(), plan, protocols, roles)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests.Measured) != 3 {
		t.Fatalf("requests=%d", len(requests.Measured))
	}
	for i, item := range requests.Measured {
		if item.ID != i || item.Label == "" {
			t.Fatalf("identity=%+v", item)
		}
	}
	if requests.Measured[0].Protocol != curlblocks.HTTP11 ||
		requests.Measured[0].Request.Header.Get("Connection") != "keep-alive" {
		t.Fatal("HTTP/1.1 ordinary fields lost")
	}
	h2 := requests.Measured[2]
	if h2.Protocol != curlblocks.HTTP2 || h2.DeclaredLength == nil || *h2.DeclaredLength != 9 || len(h2.Body) != 0 {
		t.Fatal("HTTP/2 declared length changed actual payload")
	}
	if !strings.Contains(formatTimeRequest(requests.Measured[0], false), "GET /a HTTP/1.1") ||
		!strings.Contains(formatTimeRequest(h2, false), ":method: GET") {
		t.Fatal("preview uses wrong selected protocol")
	}
}

func TestTimeRequestFramingBeforeStripping(t *testing.T) {
	for _, args := range [][]string{
		{"--http2", "-H", "Transfer-Encoding: chunked"},
		{"--http2", "-H", "TE: gzip"},
		{"--http1.1", "-H", "Content-Length: 1"},
		{"--http1.1", "-H", "Content-Length: 0", "-H", "Transfer-Encoding: chunked"},
		{"--http1.1", "-H", "Transfer-Encoding: gzip, chunked"},
		{"-X", "CONNECT"},
		{"-H", "Connection: upgrade"},
		{"-H", "Upgrade: neutral"},
		{"-H", "Content-Length: -1"},
		{"-H", "Content-Length: 0", "-H", "Content-Length: 2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			parser := curlblocks.New(curlblocks.WithDefaultScheme("https"))
			protocols := parser.ProtocolFlags(parser.Shared)
			roles := addRequestRoleFlags(parser)
			plan, err := parser.Parse(append(args, "https://one.test/"))
			if err == nil {
				_, err = materialiseTimeRequests(context.Background(), plan, protocols, roles)
			}
			if err == nil {
				t.Fatal("malformed request accepted")
			}
		})
	}
}

func TestTimePreviewPreservesFramingDeclarations(t *testing.T) {
	parser := curlblocks.New(curlblocks.WithDefaultScheme("https"))
	protocols := parser.ProtocolFlags(parser.Shared)
	roles := addRequestRoleFlags(parser)
	plan, err := parser.Parse([]string{
		"https://one.test/", "-H", "Connection: content-length, te, x-neutral",
		"-H", "Content-Length: 9", "-H", "TE: trailers", "-H", "X-Neutral: omitted",
	})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := materialiseTimeRequests(context.Background(), plan, protocols, roles)
	if err != nil {
		t.Fatal(err)
	}
	item := requests.Measured[0]
	preview := timePreviewRequest(item)
	if preview.Header.Get("Content-Length") != "9" || preview.Header.Get("TE") != "trailers" ||
		preview.Header.Get("X-Neutral") != "" || preview.Header.Get("Connection") != "" {
		t.Fatalf("preview=%v", preview.Header)
	}
	if item.Request.Header.Get("X-Neutral") != "omitted" || item.Request.Header.Get("Connection") == "" {
		t.Fatal("preview mutated authored request")
	}
}

func TestTimeHTTP11PreviewIncludesPreparedLength(t *testing.T) {
	parser := curlblocks.New(curlblocks.WithDefaultScheme("https"))
	protocols := parser.ProtocolFlags(parser.Shared)
	roles := addRequestRoleFlags(parser)
	plan, err := parser.Parse([]string{"--http1.1", "-X", "POST", "https://one.test/"})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := materialiseTimeRequests(context.Background(), plan, protocols, roles)
	if err != nil {
		t.Fatal(err)
	}
	if preview := formatTimeRequest(requests.Measured[0], false); !strings.Contains(preview, "Content-Length: 0\n") {
		t.Fatalf("prepared empty POST declaration absent: %q", preview)
	}
	if requests.Measured[0].Request.Header.Get("Content-Length") != "" {
		t.Fatal("preview changed authored fields")
	}
}

func TestTimeRequestPresentationBoundsVaryingHeaders(t *testing.T) {
	for _, protocol := range []string{"http1.1", "http2"} {
		t.Run(protocol, func(t *testing.T) {
			values := []string{strings.Repeat("first", 40), strings.Repeat("second", 40)}
			unsafe := "value\xff"
			parser := curlblocks.New(curlblocks.WithDefaultScheme("https"))
			protocols := parser.ProtocolFlags(parser.Shared)
			roles := addRequestRoleFlags(parser)
			plan, err := parser.Parse([]string{
				"--" + protocol, "--block", "https://one.test/a", "-H", "Host: override.test",
				"-H", "X-Distinguish: " + values[0], "-H", "X-Unsafe: " + unsafe, "-d", "data",
				"--block", "https://one.test/b", "-H", "Host: override.test",
				"-H", "X-Distinguish: " + values[1], "-H", "X-Unsafe: " + unsafe, "-d", "data",
			})
			if err != nil {
				t.Fatal(err)
			}
			requests, err := materialiseTimeRequests(context.Background(), plan, protocols, roles)
			if err != nil {
				t.Fatal(err)
			}
			for index, item := range requests.Measured {
				normal, verbose := formatTimeRequest(item, false), formatTimeRequest(item, true)
				name, unsafeName, length, authority := "X-Distinguish", "X-Unsafe", "Content-Length: 4", "Host: override.test"
				if protocol == "http2" {
					name, unsafeName, length, authority = "x-distinguish", "x-unsafe", "content-length: 4", ":authority: override.test"
				}
				if strings.Contains(normal, values[index]) || !strings.Contains(normal, renderHeader(name, values[index])) {
					t.Fatalf("normal varying header not bounded: %s", normal)
				}
				if !strings.Contains(normal, renderHeader(unsafeName, unsafe)) {
					t.Fatalf("unsafe header byte count absent: %s", normal)
				}
				if !strings.Contains(verbose, name+": "+values[index]) ||
					!strings.Contains(verbose, unsafeName+": "+curlblocks.DisplayText(unsafe)) {
					t.Fatalf("verbose fields incomplete: %s", verbose)
				}
				for _, text := range []string{normal, verbose} {
					if !strings.Contains(text, authority) || !strings.Contains(text, length) || !strings.Contains(text, "\ndata\n") {
						t.Fatalf("selected authority, framing or body absent: %s", text)
					}
				}
				if item.Request.Header.Get("X-Distinguish") != values[index] || item.Request.Header.Get("X-Unsafe") != unsafe {
					t.Fatal("presentation mutated caller headers")
				}
			}
		})
	}
}
