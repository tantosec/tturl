package main

import (
	"bytes"
	"testing"
	"time"
)

func TestReportReleasePlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		plan bodyReleasePlan
		want string
	}{
		{
			name: "bodyless default is omitted",
			plan: bodyReleasePlan{bodyBytesWithheld: 1},
		},
		{
			name: "explicit controls without body bytes",
			plan: bodyReleasePlan{
				releaseDelay: 5 * time.Millisecond, bodyBytesWithheld: 1,
				explicitlyConfigured: true,
			},
			want: "Body release: not applicable; requests contain no body bytes.\n",
		},
		{
			name: "zero withholding ignores delay",
			plan: bodyReleasePlan{
				releaseDelay: 5 * time.Millisecond, hasBodyBytes: true,
				explicitlyConfigured: true,
			},
			want: "Body release: no fixed tail or delay; non-empty bodies share a final record.\n",
		},
		{
			name: "withholding without delay",
			plan: bodyReleasePlan{
				bodyBytesWithheld: 1, hasBodyBytes: true,
			},
			want: "Body release: withhold up to 1 trailing byte per body; " +
				"no release delay.\n",
		},
		{
			name: "withholding before delayed release",
			plan: bodyReleasePlan{
				releaseDelay:      500 * time.Microsecond,
				bodyBytesWithheld: 8, hasBodyBytes: true,
			},
			want: "Body release: withhold up to 8 trailing bytes per body; " +
				"delay release by 500us.\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			reportReleasePlan(&output, test.plan)
			if got := output.String(); got != test.want {
				t.Errorf("report = %q, want %q", got, test.want)
			}
			assertTextLinesAtMost(t, output.String(), textWidth)
		})
	}
}
