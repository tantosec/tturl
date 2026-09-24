package tth2_test

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
	"github.com/tantosec/tturl/tth2"
)

func TestTransportPingUsesNoApplicationRequest(t *testing.T) {
	t.Parallel()
	var handlers, dials atomic.Int32
	addr, tlsConfig := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		handlers.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	dialer := &net.Dialer{}
	transport := &tth2.Transport{
		TLSClientConfig: tlsConfig,
		DialContext: func(
			ctx context.Context,
			network, address string,
		) (net.Conn, error) {
			dials.Add(1)
			return dialer.DialContext(ctx, network, address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	for range 2 {
		_, err := transport.Ping(t.Context(), addr)
		if err != nil {
			t.Fatalf("Ping: %v", err)
		}
	}
	if got := handlers.Load(); got != 0 {
		t.Errorf("handler calls = %d, want 0", got)
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("dials = %d, want one reused connection", got)
	}
}

func TestTransportPingObservesNetdelayFloor(t *testing.T) {
	t.Parallel()
	const oneWay = 20 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, tlsConfig := h2test.Serve(
		t,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("PING reached the HTTP handler")
		}),
		h2tls.WithListenerWrapper(wrapper),
	)
	transport := &tth2.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)
	rtt, err := transport.Ping(t.Context(), addr)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if rtt < 2*oneWay {
		t.Errorf("PING RTT = %v, want >= %v", rtt, 2*oneWay)
	}
}
