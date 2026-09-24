package curlblocks

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// FuzzDisplayText verifies the two compositional properties consumers rely on:
// arbitrary bytes become printable ASCII, and rendering that result again is
// stable. The latter lets callers compare typed and already-rendered labels.
func FuzzDisplayText(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "two words", " edge", "edge ", "a\x00b",
		"caf\xc3\xa9", "$HEX[00]",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got := DisplayText(raw)
		if !asciiPrintable(got) {
			t.Fatalf("DisplayText(%+q) = %+q, want printable ASCII", raw, got)
		}
		if twice := DisplayText(got); twice != got {
			t.Fatalf("DisplayText is not idempotent: first %q, second %q",
				got, twice)
		}
	})
}

// FuzzVarySourcePlan verifies the validated-plan boundary for arbitrary source
// grammar. When Parse accepts a source under a small fan ceiling, every
// deferred file check is deterministic and Expand must produce a bounded,
// consistently labelled set of groups.
func FuzzVarySourcePlan(f *testing.F) {
	for _, seed := range []string{
		"1-3", "0x00-0x0f:3", "a-z:5", "{a,b,}", "@values.txt",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 8<<10 {
			t.Skip()
		}
		readFile := func(string) ([]byte, error) {
			return []byte("alpha\r\n\n beta \nomega\n"), nil
		}
		p := New(WithFileReader(readFile), WithMaxFanSize(64))
		plan, err := p.Parse([]string{
			"https://example.test/item/T", "--vary", "T=" + source,
		})
		if err != nil {
			return
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatalf("a successfully parsed source failed to expand: %v", err)
		}
		if len(groups) < 1 || len(groups) > 64 {
			t.Fatalf("Expand produced %d groups, want 1..64", len(groups))
		}
		fan := groups[0].Fan
		if fan == nil {
			t.Fatal("expanded vary source has no Fan")
		}
		if len(fan.Variants) != len(groups) {
			t.Fatalf("Fan has %d variants across %d groups",
				len(fan.Variants), len(groups))
		}
		for i, group := range groups {
			if group.Fan != fan {
				t.Fatalf("group %d does not share the plan's Fan", i)
			}
			if group.Repeat != 1 || len(group.Labels) != 1 {
				t.Fatalf("group %d has Repeat %d and %d labels",
					i, group.Repeat, len(group.Labels))
			}
			if len(group.Block.URLs) != 1 ||
				group.Block.URLs[0] != group.URL {
				t.Fatalf("group %d URL %q disagrees with block URLs %q",
					i, group.URL, group.Block.URLs)
			}
			if len(group.Block.Vary) != 0 {
				t.Fatalf("group %d retained %d vary bindings", i,
					len(group.Block.Vary))
			}
			if !asciiPrintable(group.Labels[0].Display) {
				t.Fatalf("group %d label %+q is not printable ASCII",
					i, group.Labels[0].Display)
			}
		}
	})
}

// FuzzMergePatchIdempotent verifies an RFC 7386 property independent of any
// particular object shape: applying one merge patch twice has the same result
// as applying it once. Invalid JSON is outside the merge function's domain.
func FuzzMergePatchIdempotent(f *testing.F) {
	f.Add([]byte(`{"a":1,"nested":{"x":1}}`),
		[]byte(`{"a":null,"nested":{"y":2}}`))
	f.Add([]byte(`[1,2]`), []byte(`{"a":1}`))
	f.Add([]byte(`{"a":1}`), []byte(`[3,4]`))
	f.Fuzz(func(t *testing.T, targetJSON, patchJSON []byte) {
		if len(targetJSON) > 8<<10 || len(patchJSON) > 8<<10 {
			t.Skip()
		}
		var target, patch any
		if json.Unmarshal(targetJSON, &target) != nil ||
			json.Unmarshal(patchJSON, &patch) != nil {
			return
		}
		once := mergePatch(target, patch)
		onceJSON, err := json.Marshal(once)
		if err != nil {
			t.Fatalf("serialising first merge: %v", err)
		}

		var onceCopy, onceSemantic, patchCopy any
		if err := json.Unmarshal(onceJSON, &onceCopy); err != nil {
			t.Fatalf("decoding first merge: %v", err)
		}
		if err := json.Unmarshal(onceJSON, &onceSemantic); err != nil {
			t.Fatalf("decoding first merge comparison: %v", err)
		}
		if err := json.Unmarshal(patchJSON, &patchCopy); err != nil {
			t.Fatalf("decoding patch copy: %v", err)
		}
		twiceJSON, err := json.Marshal(mergePatch(onceCopy, patchCopy))
		if err != nil {
			t.Fatalf("serialising second merge: %v", err)
		}
		if !bytes.Equal(twiceJSON, onceJSON) {
			t.Fatalf("merge patch is not idempotent: once %s, twice %s",
				onceJSON, twiceJSON)
		}

		// The semantic comparison gives a clearer guard if JSON's canonical
		// object ordering ever changes independently of merge behaviour.
		var twice any
		if err := json.Unmarshal(twiceJSON, &twice); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(onceSemantic, twice) {
			t.Fatalf("merge results differ: once %#v, twice %#v",
				onceSemantic, twice)
		}
	})
}
