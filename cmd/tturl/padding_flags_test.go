package main

import (
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestResolvePadding(t *testing.T) {
	got, err := resolvePadding(
		"query=-2,headers=4,body=7:-2",
		[]string{"0:headers=1", "2:query=9,body=0"},
		5,
	)
	if err != nil {
		t.Fatalf("resolvePadding: %v", err)
	}
	want := []tth2.RequestPadding{
		{URLParams: 8, Headers: 1, BodyParams: 7},
		{URLParams: 6, Headers: 4, BodyParams: 5},
		{URLParams: 9, Headers: 4, BodyParams: 0},
		{URLParams: 2, Headers: 4, BodyParams: 1},
		{URLParams: 0, Headers: 4, BodyParams: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("padding length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("padding[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestResolvePaddingPositiveStep(t *testing.T) {
	got, err := resolvePadding("query=0:3", nil, 4)
	if err != nil {
		t.Fatalf("resolvePadding: %v", err)
	}
	for i, want := range []int{0, 3, 6, 9} {
		if got[i].URLParams != want {
			t.Errorf("padding[%d].URLParams = %d, want %d",
				i, got[i].URLParams, want)
		}
	}
}

func TestResolvePaddingRejectsInvalidSpecs(t *testing.T) {
	tests := []struct {
		name string
		spec string
		at   []string
		n    int
		want string
	}{
		{"unknown component", "bytes=2", nil, 2, "unknown component"},
		{"duplicate cadence", "query=1,query=2", nil, 2, "more than once"},
		{"negative start", "query=-1:-2", nil, 2, "start"},
		{"bad cadence", "query=1:2:3", nil, 2, "want N"},
		{"position out of range", "", []string{"2:query=1"}, 2, "outside"},
		{"negative override", "", []string{"0:query=-1"}, 2, "non-negative"},
		{"duplicate override", "", []string{"0:query=1", "0:query=2"}, 2, "more than once"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolvePadding(test.spec, test.at, test.n)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestPaddingCadenceRejectsOverflow(t *testing.T) {
	c := paddingCadence{start: maxInt(), step: 1}
	if _, err := c.value(1, 2); err == nil ||
		!strings.Contains(err.Error(), "maximum integer") {
		t.Errorf("error = %v, want overflow rejection", err)
	}
}

func TestResolvePaddingAllZeroIsAbsent(t *testing.T) {
	got, err := resolvePadding("query=0,headers=0,body=0", nil, 3)
	if err != nil {
		t.Fatalf("resolvePadding: %v", err)
	}
	if got != nil {
		t.Errorf("all-zero padding = %+v, want nil", got)
	}
}
