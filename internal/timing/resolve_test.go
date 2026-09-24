package timing

import (
	"crypto/tls"
	"math"
	"net/http"
	"testing"
	"time"
)

func resolveTestRequest(t *testing.T, id int, target string, protocol Protocol) Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: id, HTTP: request, Protocol: protocol}
}

func resolveTestPlan(requests ...Request) Plan {
	return Plan{
		Requests: requests, Trials: 1, Arrangement: ArrangeNone,
		SingleRecord: true, RequestTimeout: 30 * time.Second,
		ReceiveHeaderMax: 1 << 20, ResponseBodyMax: 8 << 20,
	}
}

func TestResolveAdmitsCompleteWorkersAndRotationCycles(t *testing.T) {
	requests := []Request{
		resolveTestRequest(t, 0, "https://one.test/a", HTTP2),
		resolveTestRequest(t, 1, "https://two.test/b", HTTP2),
		resolveTestRequest(t, 2, "https://one.test/c", HTTP2),
	}
	cases := []struct {
		name                                           string
		sync                                           bool
		ceiling                                        int
		trials                                         uint64
		arrange                                        Arrangement
		workers, perWorker, effective, resolvedCeiling int
		planned, units                                 uint64
	}{
		{"sequential derived", false, 0, 10, ArrangeNone, 1, 2, 2, 2, 10, 10},
		{"synchronised derived", true, 0, 10, ArrangeRandom, 1, 3, 3, 3, 10, 10},
		{"sequential spare ceiling", false, 5, 10, ArrangeNone, 2, 2, 4, 5, 10, 10},
		{"synchronised spare ceiling", true, 4, 10, ArrangeRandom, 1, 3, 3, 4, 10, 10},
		{"narrow to work", false, 10, 1, ArrangeNone, 1, 2, 2, 10, 1, 1},
		{"rotation cycles", true, 18, 4, ArrangeRotate, 2, 3, 6, 18, 6, 2},
		{"unlimited rotation", false, 6, 0, ArrangeRotate, 3, 2, 6, 6, 0, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan := resolveTestPlan(requests...)
			plan.Synchronise, plan.Connections = test.sync, test.ceiling
			plan.Trials, plan.Arrangement = test.trials, test.arrange
			resolved, err := Resolve(plan)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Width != 3 || resolved.Destinations != 2 || resolved.Workers != test.workers ||
				resolved.ConnectionsPerWorker != test.perWorker || resolved.EffectiveConnections != test.effective ||
				resolved.ConnectionCeiling != test.resolvedCeiling || resolved.PlannedTrials != test.planned ||
				resolved.WorkUnits != test.units || resolved.PlannedRequestOperations != test.planned*3 {
				t.Fatalf("incomplete allocation: %+v", resolved)
			}
		})
	}
	for _, sync := range []bool{false, true} {
		plan := resolveTestPlan(requests...)
		plan.Synchronise, plan.Connections = sync, 1
		if _, err := Resolve(plan); err == nil {
			t.Fatal("admitted partial worker")
		}
	}
	plan := resolveTestPlan(requests...)
	plan.Trials, plan.Arrangement = math.MaxUint64, ArrangeRotate
	if _, err := Resolve(plan); err == nil {
		t.Fatal("accepted overflowing planned work")
	}
}

func TestResolvePoolCompatibilityAndRequestOwnership(t *testing.T) {
	first := resolveTestRequest(t, 0, "https://EXAMPLE.test/a", HTTP2)
	first.HTTP.Host = "authority.test"
	first.HTTP.Header.Set("X-Value", "owned")
	first.Body = []byte("body")
	plan := resolveTestPlan(first,
		resolveTestRequest(t, 1, "https://example.test:443/b", HTTP2),
		resolveTestRequest(t, 2, "https://example.test/c", HTTP11),
		resolveTestRequest(t, 3, "https://example.test:8443/d", HTTP2))
	plan.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "identity.test"}
	resolved, err := Resolve(plan)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Destinations != 3 || resolved.Requests[0].PoolID != resolved.Requests[1].PoolID ||
		resolved.Pools[0].Address != "example.test:443" || resolved.Pools[0].Width != 2 {
		t.Fatalf("incorrect destination compatibility: %+v", resolved.Pools)
	}
	first.HTTP.Header.Set("X-Value", "changed")
	first.HTTP.URL.Host = "changed.test"
	first.HTTP.Host = "changed-authority.test"
	first.Body[0] = 'x'
	plan.TLSConfig.ServerName = "changed-identity.test"
	snapshot := resolved.Plan.Requests[0]
	if snapshot.HTTP.Header.Get("X-Value") != "owned" || snapshot.HTTP.URL.Host != "EXAMPLE.test" ||
		snapshot.HTTP.Host != "authority.test" || string(snapshot.Body) != "body" ||
		resolved.Plan.TLSConfig.ServerName != "identity.test" {
		t.Fatal("resolved request or TLS scalar aliases caller state")
	}
}

func TestResolveDistinctPrimingCoverage(t *testing.T) {
	a := resolveTestRequest(t, 0, "https://one.test/a", HTTP2)
	b := resolveTestRequest(t, 1, "https://two.test/b", HTTP11)
	plan := resolveTestPlan(a, b)
	plan.Warmup = 3
	plan.Priming = []Request{
		resolveTestRequest(t, 2, "https://two.test/prime", HTTP11),
		resolveTestRequest(t, 3, "https://one.test/prime", HTTP2),
	}
	resolved, err := Resolve(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Priming) != 2 || resolved.Pools[0].PrimingRequestIDs[0] != 3 ||
		resolved.Pools[1].PrimingRequestIDs[0] != 2 {
		t.Fatal("priming did not retain authored compatible projections")
	}
	for _, mutate := range []func(*Plan){
		func(p *Plan) { p.Warmup = 0 },
		func(p *Plan) { p.Priming = p.Priming[:1] },
		func(p *Plan) { p.Priming[0] = resolveTestRequest(t, 2, "https://unused.test/", HTTP2) },
		func(p *Plan) { p.Priming[0].ID = 0 },
	} {
		copyPlan := plan
		copyPlan.Priming = append([]Request(nil), plan.Priming...)
		mutate(&copyPlan)
		if _, err := Resolve(copyPlan); err == nil {
			t.Fatal("accepted invalid distinct priming set")
		}
	}
}

func TestResolveProtocolFramingBeforeInteraction(t *testing.T) {
	for _, protocol := range []Protocol{HTTP11, HTTP2} {
		request := resolveTestRequest(t, 0, "https://example.test/", protocol)
		request.Body = []byte("x")
		request.HTTP.Header.Set("Content-Length", "9")
		resolved, err := Resolve(resolveTestPlan(request))
		if protocol == HTTP11 {
			if err == nil {
				t.Fatal("accepted HTTP/1.1 length mismatch")
			}
		} else if err != nil || !resolved.Requests[0].LengthMismatch ||
			resolved.Requests[0].DeclaredContentLength.Value != 9 {
			t.Fatalf("lost deliberate HTTP/2 declaration: resolved=%+v err=%v", resolved, err)
		}
		request.HTTP.Header.Set("Content-Length", "1")
		if _, err := Resolve(resolveTestPlan(request)); err != nil {
			t.Fatal(err)
		}
		request.HTTP.Header.Add("Content-Length", "2")
		if _, err := Resolve(resolveTestPlan(request)); err == nil {
			t.Fatal("accepted conflicting lengths")
		}
	}
	request := resolveTestRequest(t, 0, "https://example.test/", HTTP2)
	for _, header := range []http.Header{
		{"Transfer-Encoding": {"chunked"}},
		{"Connection": {"Upgrade"}, "Upgrade": {"other"}},
		{"Connection": {"X-Discard"}, "X-Discard": {"bad\r\nvalue"}},
	} {
		request.HTTP.Header = header
		if _, err := Resolve(resolveTestPlan(request)); err == nil {
			t.Fatal("stripped prohibited HTTP/2 framing")
		}
	}
	request = resolveTestRequest(t, 0, "http://example.test/", HTTP11)
	plan := resolveTestPlan(request)
	if _, err := Resolve(plan); err == nil {
		t.Fatal("weakened TLS record promise for plain HTTP")
	}
	plan.SingleRecord = false
	if _, err := Resolve(plan); err != nil {
		t.Fatal(err)
	}
	plan.LastByteSync = true
	if _, err := Resolve(plan); err == nil {
		t.Fatal("accepted plain HTTP last-byte promise")
	}
}
