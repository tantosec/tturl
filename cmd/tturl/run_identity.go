package main

import (
	"fmt"
	"strconv"
)

// runReference is one request's complete identity and its repeated inline
// representation in a projection chosen for one report layout.
type runReference struct {
	ID     int
	Label  string
	Inline string
}

// runReferences is one run-wide projection. Compact reports whether every
// inline identity carries its ID because a complete label exceeds the
// reporter's allowance.
type runReferences struct {
	refs    []runReference
	compact bool
}

func projectRunReferences(labels []string, maxWidth int) runReferences {
	compact := false
	for _, label := range labels {
		if len(label) > maxWidth {
			compact = true
			break
		}
	}
	out := runReferences{
		refs:    make([]runReference, len(labels)),
		compact: compact,
	}
	for id, label := range labels {
		inline := label
		if compact {
			prefix := "#" + strconv.Itoa(id) + " "
			available := maxWidth - len(prefix)
			if available < 3 {
				panic(fmt.Sprintf(
					"request reference width %d cannot hold request ID %d and an excerpt",
					maxWidth, id))
			}
			inline = prefix + capMiddle(label, available)
		}
		out.refs[id] = runReference{ID: id, Label: label, Inline: inline}
	}
	return out
}

func (r runReferences) inlineLabels() []string {
	out := make([]string, len(r.refs))
	for i := range r.refs {
		out[i] = r.refs[i].Inline
	}
	return out
}

func (r runReferences) projectRankView(view rankView) rankView {
	if len(view.pinned) == 0 {
		return view
	}
	pinned := make(map[string]bool, len(view.pinned))
	for _, ref := range r.refs {
		if view.pinned[ref.Label] {
			pinned[ref.Inline] = true
		}
	}
	view.pinned = pinned
	return view
}
