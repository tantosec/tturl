package ranking

import "testing"

func TestBuiltInOutlierMethodIDs(t *testing.T) {
	solvers := []IdentifiedOutlierSolver{
		&PeerFirstOutlierSolver{},
		&RollingPeerFirstOutlierSolver{},
		&BaselineConfirmedOutlierSolver{},
		&RollingBaselineConfirmedOutlierSolver{},
		&BaselineReservedOutlierSolver{},
		&RollingBaselineReservedOutlierSolver{},
		&EdgeDirectedOutlierSolver{},
	}
	seen := make(map[OutlierMethodID]bool, len(solvers))
	for _, solver := range solvers {
		id := solver.MethodID()
		if !id.Valid() {
			t.Errorf("MethodID %q is invalid", id)
		}
		if seen[id] {
			t.Errorf("MethodID %q is not distinct", id)
		}
		seen[id] = true
	}
}

func TestOutlierMethodIDLexicalContract(t *testing.T) {
	for _, test := range []struct {
		id   OutlierMethodID
		want bool
	}{
		{"ranking/outlier/example/1", true},
		{"with space", true},
		{"", false},
		{"contains\nnewline", false},
		{OutlierMethodID(string([]byte{0x7f})), false},
		{"non-ASCII-\u00e9", false},
	} {
		if got := test.id.Valid(); got != test.want {
			t.Errorf("OutlierMethodID(%q).Valid() = %t, want %t",
				test.id, got, test.want)
		}
	}
}
