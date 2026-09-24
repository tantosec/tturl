package curlblocks

import (
	"strings"
	"testing"
)

// TestResetBody checks a block's --reset-body: it discards any body inherited
// from the preamble (so the block is bodyless unless it sets its own), lets a
// block set a different body kind than the preamble without the one-body
// conflict, and leaves inherited headers untouched. It is block-scoped.
func TestResetBody(t *testing.T) {
	t.Run("drops the inherited body; block goes bodyless but keeps headers", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"--json", `{"env":"base"}`, "-H", "X-Shared: yes",
			"--block", "https://a",
			"--block", "https://b", "--reset-body",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if len(plan.Blocks[0].json) == 0 {
			t.Error("block A lost the inherited JSON body")
		}
		b := plan.Blocks[1]
		if len(b.json) != 0 || len(b.data) != 0 || len(b.form) != 0 {
			t.Errorf("block B still has a body: json=%v data=%v form=%v", b.json, b.data, b.form)
		}
		if b.HTTPMethod() != "GET" {
			t.Errorf("block B method = %q, want GET (bodyless after reset)", b.HTTPMethod())
		}
		if !b.setsHeader("X-Shared") {
			t.Error("block B lost the inherited header; --reset-body must not touch headers")
		}
		if _, present, _, err := b.RequestBody(); err != nil || present {
			t.Errorf("block B RequestBody present=%v err=%v, want no body", present, err)
		}
	})

	t.Run("allows a different body kind than the preamble", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"--json", `{"a":1}`,
			"--block", "https://a", "--reset-body", "-F", "f=v",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		b := plan.Blocks[0]
		if len(b.json) != 0 {
			t.Errorf("reset block still carries inherited json: %v", b.json)
		}
		if len(b.form) != 1 {
			t.Errorf("reset block missing its own form: %v", b.form)
		}
	})

	t.Run("a block --json after reset does not inherit the envelope", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"--json", `{"op":"x","id":"AAAA"}`,
			"--block", "https://a", "--reset-body", "--json", `{"id":"BBBB"}`,
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		body, present, _, err := plan.Blocks[0].RequestBody()
		if err != nil || !present {
			t.Fatalf("RequestBody err=%v present=%v", err, present)
		}
		// Only the block's own document, verbatim — no inherited "op" merged in.
		if string(body) != `{"id":"BBBB"}` {
			t.Errorf("reset block body = %q, want just the block's own document", body)
		}
	})

	t.Run("not valid in the preamble", func(t *testing.T) {
		_, err := New().Parse([]string{"--reset-body", "https://a"})
		if err == nil || !strings.Contains(err.Error(), "not valid in the preamble") {
			t.Fatalf("err = %v, want a preamble-scope error", err)
		}
	})
}

// TestFileRefsRejectedAtParse checks that the two faults in a file reference
// that need no reading to spot — an empty name and stdin — are raised by
// Parse,
// for every flag family that takes one. Each error names the flag as the user
// spelled it, and the '@' or '<' they wrote.
func TestFileRefsRejectedAtParse(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		errSub string
	}{
		{"-d stdin", []string{"-d", "@-", "https://a"}, "--data: reading from stdin ('@-')"},
		{"--data-binary stdin", []string{"--data-binary", "@-", "https://a"}, "--data-binary: reading from stdin"},
		{"--data-urlencode stdin", []string{"--data-urlencode", "n@-", "https://a"}, "--data-urlencode: reading from stdin"},
		{"-d empty name", []string{"-d", "@", "https://a"}, "--data: empty filename after '@'"},
		{"-F upload stdin", []string{"-F", "f=@-", "https://a"}, "-F: reading from stdin ('@-')"},
		{"-F field-from-file stdin", []string{"-F", "f=<-", "https://a"}, "-F: reading from stdin ('<-')"},
		{"--json stdin", []string{"--json", "@-", "https://a"}, "--json: reading from stdin"},
		{"--vary stdin", []string{"--vary", "T=@-", "https://a/T"}, "--vary T: reading from stdin ('@-')"},
		{"--vary empty name", []string{"--vary", "T=@", "https://a/T"}, "--vary T: empty filename after '@'"},
		// --data-raw takes its argument literally, so a leading '@' is content.
		{"--data-raw keeps a literal @-", []string{"--data-raw", "@-", "https://a"}, ""},
		// A '=' anywhere in --data-urlencode makes a later '@' literal too.
		{"--data-urlencode inline content keeps a literal @-", []string{"--data-urlencode", "n=@-", "https://a"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Parse(tt.args)
			switch {
			case tt.errSub == "" && err != nil:
				t.Fatalf("parse error = %v, want none", err)
			case tt.errSub == "":
			case err == nil || !strings.Contains(err.Error(), tt.errSub):
				t.Fatalf("err = %v, want one containing %q", err, tt.errSub)
			}
		})
	}
}

// TestInlineJSONMergeCheckedAtParse checks that the merge's object requirement
// is settled by Parse when every --json piece is inline: nothing is left to
// read, so the user hears about a non-object base or patch with their other
// command-line mistakes rather than at RequestBody.
func TestInlineJSONMergeCheckedAtParse(t *testing.T) {
	tests := []struct {
		name, base, patch, errSub string
	}{
		{"non-object base", `[1,2]`, `{"a":1}`, "base --json must be a JSON object"},
		{"non-object patch", `{"a":1}`, `[3,4]`, "--json patch must be a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Parse([]string{"--json", tt.base, "--json", tt.patch, "https://a"})
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Fatalf("err = %v, want one containing %q", err, tt.errSub)
			}
		})
	}
}
