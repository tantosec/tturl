package tth2

import (
	"strings"
	"testing"
)

// TestAllocateStreamIDs covers the fresh-allocation case and the
// boundary-then-exhaustion case for the stream-ID guard. Client streams use odd
// IDs ≤ 2^31-1; once the allocator would emit an ID past that cap it must
// refuse, and Client.send's retry path treats the refusal as a transport-level
// fault that triggers a re-dial.
func TestAllocateStreamIDs(t *testing.T) {
	t.Parallel()

	c := &http2conn{nextStreamID: 1}
	ids, err := c.allocateStreamIDs(3)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if got, want := ids, []uint32{1, 3, 5}; !equalU32(got, want) {
		t.Errorf("first batch IDs: got %v, want %v", got, want)
	}
	if c.nextStreamID != 7 {
		t.Errorf("nextStreamID after first batch: got %d, want 7", c.nextStreamID)
	}

	// Push to the boundary: with nextStreamID = maxStreamID-2 = 2^31-3 (odd), a
	// batch of 2 emits IDs maxStreamID-2 and maxStreamID, which is exactly the
	// allowed range.
	c.nextStreamID = uint32(maxStreamID - 2)
	ids, err = c.allocateStreamIDs(2)
	if err != nil {
		t.Fatalf("boundary batch: %v", err)
	}
	if got, want := ids, []uint32{uint32(maxStreamID - 2), uint32(maxStreamID)}; !equalU32(got, want) {
		t.Errorf("boundary IDs: got %v, want %v", got, want)
	}

	// One past the boundary must fail.
	if _, err := c.allocateStreamIDs(1); err == nil {
		t.Fatal("post-boundary allocation: expected exhaustion error, got nil")
	} else if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("post-boundary error %q does not mention exhaustion", err)
	}

	// n=0 is a no-op, returning a nil slice and no error.
	c2 := &http2conn{nextStreamID: 1}
	ids, err = c2.allocateStreamIDs(0)
	if err != nil {
		t.Errorf("n=0: %v", err)
	}
	if ids != nil {
		t.Errorf("n=0: got %v, want nil", ids)
	}
	if c2.nextStreamID != 1 {
		t.Errorf("n=0: nextStreamID changed to %d", c2.nextStreamID)
	}

	// A single batch that crosses the boundary mid-list must also fail (and must
	// not partially mutate nextStreamID).
	c3 := &http2conn{nextStreamID: uint32(maxStreamID)}
	if _, err := c3.allocateStreamIDs(2); err == nil {
		t.Fatal("crossing-boundary batch: expected error, got nil")
	}
	if c3.nextStreamID != uint32(maxStreamID) {
		t.Errorf("crossing-boundary batch mutated nextStreamID to %d", c3.nextStreamID)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
