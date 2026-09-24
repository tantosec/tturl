package ranking

import "testing"

func assertGuarantee(
	t *testing.T,
	got OutlierGuarantee,
	want OutlierGuarantee,
) {
	t.Helper()
	if got != want {
		t.Errorf("guarantee = %+v, want %+v", got, want)
	}
}
