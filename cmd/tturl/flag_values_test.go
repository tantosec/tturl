package main

import (
	"testing"
	"time"
)

func TestParseDurationOrUnlimited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value     string
		want      time.Duration
		unlimited bool
		wantErr   bool
	}{
		{value: "0"},
		{value: "2.5s", want: 2500 * time.Millisecond},
		{value: "-1ns", want: -time.Nanosecond},
		{value: "unlimited", unlimited: true},
		{value: "UNLIMITED", unlimited: true},
		{value: "forever", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			got, unlimited, err := parseDurationOrUnlimited(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("got %s, unlimited %t; want an error",
						got, unlimited)
				}
				return
			}
			if err != nil || got != test.want || unlimited != test.unlimited {
				t.Errorf("got %s, unlimited %t, %v; want %s, unlimited %t",
					got, unlimited, err, test.want, test.unlimited)
			}
		})
	}
}
