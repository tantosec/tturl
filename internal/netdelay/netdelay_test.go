package netdelay

import (
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNewListenerWrapperRejectsNegativeDurations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{name: "delay", config: Config{Delay: -1}, want: "delay"},
		{name: "variation", config: Config{Variation: -1}, want: "variation"},
		{
			name: "variation too large",
			config: Config{
				Variation: maximumDuration,
			},
			want: "too large",
		},
		{
			name: "overflow",
			config: Config{
				Delay: maximumDuration, Variation: time.Nanosecond,
			},
			want: "overflows",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewListenerWrapper(test.config); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Errorf("NewListenerWrapper(%+v) error = %v, want %q",
					test.config, err, test.want)
			}
		})
	}
}

func TestNewListenerWrapperZeroIsIdentity(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(
		t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	wrapper, err := NewListenerWrapper(Config{})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	if got := wrapper(listener); got != listener {
		t.Errorf("zero-config wrapper returned %T, want original listener", got)
	}
}

func TestNewListenerWrapperSeedResolution(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(
		t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for _, test := range []struct {
		name string
		seed uint64
		want uint64
	}{
		{name: "random zero", seed: 0},
		{name: "explicit", seed: 42, want: 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wrapper, err := NewListenerWrapper(Config{
				Variation: time.Nanosecond, Seed: test.seed,
			})
			if err != nil {
				t.Fatalf("NewListenerWrapper: %v", err)
			}
			wrapped, ok := wrapper(listener).(*delayListener)
			if !ok {
				t.Fatalf("wrapped listener = %T, want *delayListener", wrapped)
			}
			if test.want == 0 {
				if wrapped.config.Seed == 0 {
					t.Fatal("zero seed was not randomised")
				}
				return
			}
			if wrapped.config.Seed != test.want {
				t.Errorf("resolved seed = %d, want %d",
					wrapped.config.Seed, test.want)
			}
		})
	}
}

func TestSampleDelayIsBoundedAndRepeatable(t *testing.T) {
	t.Parallel()
	config := Config{Delay: 50 * time.Millisecond, Variation: 10 * time.Millisecond}
	rng := rand.New( //nolint:gosec // deterministic simulation test
		rand.NewPCG(1, 2))
	replay := rand.New( //nolint:gosec // deterministic simulation test
		rand.NewPCG(1, 2))
	seen := make(map[time.Duration]bool)
	for range 1000 {
		delay := sampleDelay(rng, config)
		if want := sampleDelay(replay, config); delay != want {
			t.Fatalf("delay = %s, replay = %s", delay, want)
		}
		if delay < config.Delay || delay > config.Delay+config.Variation {
			t.Fatalf("delay = %s, want 50ms through 60ms", delay)
		}
		seen[delay] = true
	}
	if len(seen) < 2 {
		t.Fatal("variation produced only one delay")
	}
}

func TestScheduleDeliveryPreservesOrder(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	last := now.Add(20 * time.Millisecond)
	if got := scheduleDelivery(now, last, time.Millisecond); !got.Equal(last) {
		t.Errorf("clamped delivery = %s, want %s", got, last)
	}
	want := now.Add(30 * time.Millisecond)
	if got := scheduleDelivery(now, last, 30*time.Millisecond); !got.Equal(want) {
		t.Errorf("later delivery = %s, want %s", got, want)
	}
}

func TestListenerDelaysBothDirections(t *testing.T) {
	raw, err := (&net.ListenConfig{}).Listen(
		t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	wrapper, err := NewListenerWrapper(Config{
		Delay: 20 * time.Millisecond,
		Seed:  1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	listener := wrapper(raw)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := (&net.Dialer{}).DialContext(
		t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var server net.Conn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(time.Second):
		t.Fatal("accept timed out")
	}
	t.Cleanup(func() { _ = server.Close() })

	assertDirection := func(name string, writeTo, readFrom net.Conn) {
		t.Helper()
		start := time.Now()
		if _, err := writeTo.Write([]byte{'x'}); err != nil {
			t.Fatalf("%s write: %v", name, err)
		}
		if err := readFrom.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("%s deadline: %v", name, err)
		}
		var got [1]byte
		if _, err := io.ReadFull(readFrom, got[:]); err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		elapsed := time.Since(start)
		if elapsed < 15*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Errorf("%s elapsed = %s, want delayed by about 20ms", name, elapsed)
		}
	}
	assertDirection("client to server", client, server)
	assertDirection("server to client", server, client)
	if server.LocalAddr().String() != client.RemoteAddr().String() ||
		server.RemoteAddr().String() != client.LocalAddr().String() {
		t.Errorf("server addresses = %s -> %s, client = %s -> %s",
			server.LocalAddr(), server.RemoteAddr(),
			client.LocalAddr(), client.RemoteAddr())
	}
}
