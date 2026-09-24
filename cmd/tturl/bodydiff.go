package main

import (
	"bytes"
	"fmt"
	"strings"
)

// The body digest condenses a batch of request bodies to one short line each,
// showing what distinguishes a body from its siblings. It is a segment diff:
//
//   - segmentDiff finds the byte runs common to every body (anchors) and the
//     runs between them where at least one body differs (changes), by recursive
//     common-prefix/suffix peeling plus an interior longest-common-substring
//     anchor. The result is the batch's skeleton: an alternating sequence of
//     shared anchors and change regions, in order.
//   - renderDigest walks a body across that skeleton, showing its content in
//     each change region with a little shared context on either side, eliding
//     shared runs with "...", and - when the changes don't all fit - stopping
//     and appending "...(+N more differences)" so hidden differences are
//     counted where they are dropped.
//
// The unit is a changed *region*, not a byte: a value that changes length (4
// bytes to 3) is one change, and end-anchoring keeps the realignment after it
// exact. Multiple scattered changes each get their own region, so two bodies
// that differ only deep in the middle render as distinct lines.

const (
	// minAnchor is the shortest shared run treated as an anchor separating two
	// changes; below it a coincidental match is folded into the change rather
	// than splitting it.
	minAnchor = 3
	// maxScan bounds the differing region searched for an interior anchor, so a
	// pathologically large change can't make the diff expensive; a larger region
	// is left as one change (its head and tail still show).
	maxScan = 8192
	// hashBase seeds the rolling hash used to find common substrings. Any odd
	// multiplier works; this is the FNV-1a 64-bit prime.
	hashBase = 1099511628211
)

// bodySegment is one region of the batch skeleton. change is true where the
// bodies differ and false for a run shared by all; spans holds, per body, the
// [start,end) byte range the region occupies in that body (an anchor's spans
// are byte-identical in content, a change's hold each body's differing bytes).
type bodySegment struct {
	change bool
	spans  [][2]int
}

// bodyDigests returns an index-aligned novelty view of each non-empty printable
// body. It extends initially identical renderings through a distinguishing
// region when possible. Empty or unsafe bodies receive an empty digest.
func bodyDigests(bodies [][]byte) []string {
	out := make([]string, len(bodies))
	var present []int
	for i, b := range bodies {
		if len(b) > 0 {
			present = append(present, i)
		}
	}
	switch len(present) {
	case 0:
		return out
	case 1:
		if d, ok := excerpt(bodies[present[0]]); ok {
			out[present[0]] = d
		}
		return out
	}
	pb := make([][]byte, len(present))
	for j, i := range present {
		pb[j] = bodies[i]
	}
	segs := segmentDiff(pb)
	var changeSegs []bodySegment
	for _, s := range segs {
		if s.change {
			changeSegs = append(changeSegs, s)
		}
	}

	n := len(pb)
	texts := make([]string, n)
	shown := make([]int, n)
	okv := make([]bool, n)
	minShow := make([]int, n)
	render := func(j int) { texts[j], shown[j], okv[j] = renderDigest(pb[j], j, segs, minShow[j]) }
	for j := range pb {
		render(j)
	}

	// Reconciliation: extend a colliding pair to show the first change region
	// where they differ, until no two different bodies share a line. Each fix
	// strictly grows a body's shown prefix, so the loop terminates; a pair whose
	// difference is only inside an already-shown region is marked unresolvable so
	// it is not retried.
	unresolvable := map[[2]int]bool{}
	for {
		progressed := false
		for a := 0; a < n && !progressed; a++ {
			for b := a + 1; b < n; b++ {
				if !okv[a] || !okv[b] || texts[a] != texts[b] {
					continue
				}
				if bytes.Equal(pb[a], pb[b]) { // identical bodies: one line is correct
					continue
				}
				if unresolvable[[2]int{a, b}] {
					continue
				}
				d := firstDiffChange(pb, changeSegs, a, b)
				if d < 0 || (d < shown[a] && d < shown[b]) {
					unresolvable[[2]int{a, b}] = true
					continue
				}
				need := d + 1
				if minShow[a] < need {
					minShow[a] = need
					render(a)
				}
				if minShow[b] < need {
					minShow[b] = need
					render(b)
				}
				progressed = true
				break
			}
		}
		if !progressed {
			break
		}
	}

	for j, i := range present {
		if okv[j] {
			out[i] = texts[j]
		}
	}
	return out
}

// firstDiffChange returns the index of the first change region whose content
// differs between bodies a and b, or -1 if they match in every change region
// (which, since anchors are byte-identical across all bodies, means the bodies
// are equal).
func firstDiffChange(pb [][]byte, changeSegs []bodySegment, a, b int) int {
	for r, cs := range changeSegs {
		sa, sb := cs.spans[a], cs.spans[b]
		if !bytes.Equal(pb[a][sa[0]:sa[1]], pb[b][sb[0]:sb[1]]) {
			return r
		}
	}
	return -1
}

// segmentDiff computes the batch skeleton for two or more bodies: the ordered
// sequence of shared anchors and change regions covering every body.
func segmentDiff(bodies [][]byte) []bodySegment {
	ranges := make([][2]int, len(bodies))
	for i := range bodies {
		ranges[i] = [2]int{0, len(bodies[i])}
	}
	var out []bodySegment
	splitRegion(bodies, ranges, &out)
	return out
}

// splitRegion diffs the given sub-ranges and appends their segments to out, in
// order. It peels the shared prefix and suffix as anchors, then splits the
// differing middle at its longest interior anchor and recurses; a middle with
// no anchor (or too large to scan) becomes a single change region.
func splitRegion(bodies [][]byte, ranges [][2]int, out *[]bodySegment) {
	// Own copy: peeling the prefix advances these bounds, and some of the slices a
	// caller passes are also the spans it stores in out.
	r := make([][2]int, len(ranges))
	copy(r, ranges)

	if p := commonPrefixInRanges(bodies, r); p > 0 {
		anc := make([][2]int, len(r))
		for i := range r {
			anc[i] = [2]int{r[i][0], r[i][0] + p}
		}
		*out = append(*out, bodySegment{spans: anc})
		for i := range r {
			r[i][0] += p
		}
	}

	// The suffix anchor is found now but emitted after the middle, to keep order.
	s := commonSuffixInRanges(bodies, r)
	suffix := make([][2]int, len(r))
	mid := make([][2]int, len(r))
	for i := range r {
		suffix[i] = [2]int{r[i][1] - s, r[i][1]}
		mid[i] = [2]int{r[i][0], r[i][1] - s}
	}

	if !allEmptyRanges(mid) {
		var starts []int
		anchorLen := 0
		if withinScan(mid) {
			starts, anchorLen = longestCommonSubstring(bodies, mid)
		}
		if anchorLen >= minAnchor {
			left := make([][2]int, len(mid))
			anc := make([][2]int, len(mid))
			right := make([][2]int, len(mid))
			for i := range mid {
				a := starts[i]
				left[i] = [2]int{mid[i][0], a}
				anc[i] = [2]int{a, a + anchorLen}
				right[i] = [2]int{a + anchorLen, mid[i][1]}
			}
			splitRegion(bodies, left, out)
			*out = append(*out, bodySegment{spans: anc})
			splitRegion(bodies, right, out)
		} else {
			*out = append(*out, bodySegment{change: true, spans: mid})
		}
	}

	if s > 0 {
		*out = append(*out, bodySegment{spans: suffix})
	}
}

// commonPrefixInRanges returns the length of the byte prefix shared by every
// body over its given sub-range.
func commonPrefixInRanges(bodies [][]byte, ranges [][2]int) int {
	maxP := ranges[0][1] - ranges[0][0]
	for i := 1; i < len(ranges); i++ {
		maxP = min(maxP, ranges[i][1]-ranges[i][0])
	}
	p := 0
	for p < maxP {
		c := bodies[0][ranges[0][0]+p]
		for i := 1; i < len(bodies); i++ {
			if bodies[i][ranges[i][0]+p] != c {
				return p
			}
		}
		p++
	}
	return p
}

// commonSuffixInRanges returns the length of the byte suffix shared by every
// body over its given sub-range.
func commonSuffixInRanges(bodies [][]byte, ranges [][2]int) int {
	maxS := ranges[0][1] - ranges[0][0]
	for i := 1; i < len(ranges); i++ {
		maxS = min(maxS, ranges[i][1]-ranges[i][0])
	}
	s := 0
	for s < maxS {
		c := bodies[0][ranges[0][1]-1-s]
		for i := 1; i < len(bodies); i++ {
			if bodies[i][ranges[i][1]-1-s] != c {
				return s
			}
		}
		s++
	}
	return s
}

// longestCommonSubstring finds the longest byte substring present in every
// body's sub-range, returning its start per body (leftmost in body 0, so the
// result is deterministic) and its length, or (nil, 0) if none reaches
// minAnchor. Length is found by binary search - feasibility is monotone, since
// any common L-substring contains a common (L-1)-substring.
func longestCommonSubstring(bodies [][]byte, ranges [][2]int) ([]int, int) {
	minLen := ranges[0][1] - ranges[0][0]
	for i := 1; i < len(ranges); i++ {
		minLen = min(minLen, ranges[i][1]-ranges[i][0])
	}
	if minLen < minAnchor {
		return nil, 0
	}
	lo, hi := minAnchor, minLen
	var bestStarts []int
	best := 0
	for lo <= hi {
		L := (lo + hi) / 2
		if starts, ok := commonSubstringOfLen(bodies, ranges, L); ok {
			best, bestStarts = L, starts
			lo = L + 1
		} else {
			hi = L - 1
		}
	}
	return bestStarts, best
}

// commonSubstringOfLen returns the positions of the leftmost length-L substring
// from body zero that occurs in every range. Hash candidates are verified by
// byte comparison, so collisions cannot produce a false match.
func commonSubstringOfLen(bodies [][]byte, ranges [][2]int, L int) ([]int, bool) {
	others := make([]map[uint64]int, len(bodies))
	for bi := 1; bi < len(bodies); bi++ {
		others[bi] = rollHashes(bodies[bi], ranges[bi], L)
		if len(others[bi]) == 0 {
			return nil, false
		}
	}
	starts := make([]int, len(bodies))
	for start, h := range rollHashSeq(bodies[0], ranges[0], L) {
		gram := bodies[0][start : start+L]
		starts[0] = start
		ok := true
		for bi := 1; bi < len(bodies); bi++ {
			st, found := others[bi][h]
			if !found || !bytes.Equal(bodies[bi][st:st+L], gram) {
				ok = false
				break
			}
			starts[bi] = st
		}
		if ok {
			return starts, true
		}
	}
	return nil, false
}

// rollHashSeq yields the start offset and rolling hash of each length-L
// substring of body b over range r, left to right. It yields nothing when L
// does not fit the range.
func rollHashSeq(b []byte, r [2]int, L int) func(func(int, uint64) bool) {
	return func(yield func(int, uint64) bool) {
		if L <= 0 || L > r[1]-r[0] {
			return
		}
		var pow uint64 = 1
		for range L - 1 {
			pow *= hashBase
		}
		var h uint64
		for i := r[0]; i < r[0]+L; i++ {
			h = h*hashBase + uint64(b[i])
		}
		if !yield(r[0], h) {
			return
		}
		for i := r[0] + L; i < r[1]; i++ {
			h -= uint64(b[i-L]) * pow
			h = h*hashBase + uint64(b[i])
			if !yield(i-L+1, h) {
				return
			}
		}
	}
}

// rollHashes returns, for body b over range r, the leftmost start of every
// distinct length-L substring keyed by its rolling hash. Empty when L does not
// fit the range.
func rollHashes(b []byte, r [2]int, L int) map[uint64]int {
	m := map[uint64]int{}
	for start, h := range rollHashSeq(b, r, L) {
		if _, ok := m[h]; !ok {
			m[h] = start
		}
	}
	return m
}

// renderDigest renders change regions with bounded shared context. minShow can
// force additional regions past the normal budget. It returns the number shown;
// ok is false if any shown byte is unsafe to print.
func renderDigest(body []byte, idx int, segs []bodySegment, minShow int) (string, int, bool) {
	var changes [][2]int
	for _, s := range segs {
		if s.change {
			changes = append(changes, s.spans[idx])
		}
	}
	if len(changes) == 0 {
		s, ok := excerpt(body)
		return s, 0, ok
	}

	total := len(changes)
	pieces := make([]string, total)
	for k := range changes {
		var gap string
		var ok bool
		if k == 0 {
			gap, ok = leadingGap(body[:changes[0][0]])
		} else {
			gap, ok = interiorGap(body[changes[k-1][1]:changes[k][0]])
		}
		if !ok {
			return "", 0, false
		}
		content, ok := sanitiseInline(body[changes[k][0]:changes[k][1]])
		if !ok {
			return "", 0, false
		}
		pieces[k] = gap + capMiddle(content, maxDigest)
	}
	trailing, ok := trailingGap(body[changes[total-1][1]:])
	if !ok {
		return "", 0, false
	}

	var sb strings.Builder
	shown := 0
	for k := range total {
		forced := k < minShow
		if !forced && shown > 0 && sb.Len()+len(pieces[k]) > maxDigest {
			break
		}
		sb.WriteString(pieces[k])
		shown++
	}
	if rem := total - shown; rem > 0 {
		word := "differences"
		if rem == 1 {
			word = "difference"
		}
		fmt.Fprintf(&sb, " ...(+%d more %s)", rem, word)
	} else {
		sb.WriteString(trailing)
	}
	return sb.String(), shown, true
}

// excerpt renders a lone or all-identical body: a bounded head-and-tail view,
// with no diff to focus on.
func excerpt(body []byte) (string, bool) {
	s, ok := sanitiseInline(body)
	if !ok {
		return "", false
	}
	return capMiddle(s, maxDigest), true
}

// contextBytes is how much of the shared text on each side of a body's
// distinguishing span the digest keeps, so a differing value shows with the key
// or delimiter that gives it meaning (e.g. ..."sleep_us":50} not a bare 50).
const contextBytes = 20

// leadingGap renders the shared run before the first change as leading context:
// its last contextBytes, prefixed with "..." when more was elided.
func leadingGap(shared []byte) (string, bool) {
	seg, trunc := shared, false
	if len(shared) > contextBytes {
		seg, trunc = shared[len(shared)-contextBytes:], true
	}
	s, ok := sanitiseInline(seg)
	if !ok {
		return "", false
	}
	if trunc {
		s = "..." + s
	}
	return s, true
}

// trailingGap renders the shared run after the last change as trailing context:
// its first contextBytes, suffixed with "..." when more was elided.
func trailingGap(shared []byte) (string, bool) {
	seg, trunc := shared, false
	if len(shared) > contextBytes {
		seg, trunc = shared[:contextBytes], true
	}
	s, ok := sanitiseInline(seg)
	if !ok {
		return "", false
	}
	if trunc {
		s += "..."
	}
	return s, true
}

// interiorGap renders a shared run between two changes: trailing context for
// the change before it and leading context for the one after, joined by "..."
// when the run is longer than both contexts together.
func interiorGap(shared []byte) (string, bool) {
	if len(shared) <= 2*contextBytes {
		return sanitiseInline(shared)
	}
	head, ok := sanitiseInline(shared[:contextBytes])
	if !ok {
		return "", false
	}
	tail, ok := sanitiseInline(shared[len(shared)-contextBytes:])
	if !ok {
		return "", false
	}
	return head + "..." + tail, true
}

// allEmptyRanges reports whether every sub-range is empty.
func allEmptyRanges(ranges [][2]int) bool {
	for _, r := range ranges {
		if r[1] > r[0] {
			return false
		}
	}
	return true
}

// withinScan reports whether every sub-range is short enough to search for an
// interior anchor (see maxScan).
func withinScan(ranges [][2]int) bool {
	for _, r := range ranges {
		if r[1]-r[0] > maxScan {
			return false
		}
	}
	return true
}
