package tth2

import (
	"math"
	"strings"
	"testing"
)

func TestTransportResponseHeaderBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     int64
		want      uint32
		advertise bool
		wantError string
	}{
		{name: "default", want: 1 << 20, advertise: true},
		{name: "finite", value: 37, want: 37, advertise: true},
		{
			name:  "unlimited",
			value: UnlimitedResponseBytes,
			want:  math.MaxUint32,
		},
		{name: "invalid", value: -2, wantError: ">= -1"},
		{
			name:      "unrepresentable",
			value:     int64(math.MaxUint32) + 1,
			wantError: "exceeds",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Transport{MaxResponseHeaderBytes: tc.value}
			got, advertise, err := tr.responseHeaderBytes()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("responseHeaderBytes: %v", err)
			}
			if got != tc.want || advertise != tc.advertise {
				t.Errorf("got (%d, %t), want (%d, %t)",
					got, advertise, tc.want, tc.advertise)
			}
		})
	}
}
