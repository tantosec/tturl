package main

// rankView controls the rows retained in a text ranking projection. endRows
// keeps that many rows at each sorted end; pinned labels remain visible; and
// allRows disables elision. It never changes acquisition or structured output.
type rankView struct {
	endRows int
	pinned  map[string]bool
	allRows bool
}
