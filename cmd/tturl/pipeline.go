package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/tantosec/tturl/tth2"
)

const (
	pathPingSamples = 3
	// The automatic-fit thresholds are review budgets, not safe target loads,
	// host-resource guarantees, or signal-fidelity limits.
	automaticFitReviewMaximumConnections = 512
	automaticFitReviewMaximumStreams     = 1024
)

type connectionFit string

const (
	connectionDefault        connectionFit = "default"
	connectionExplicit       connectionFit = "explicit"
	connectionPathPing       connectionFit = "path_ping"
	connectionPathPingCapped connectionFit = "path_ping_capped"
)

func fitPipeline(
	ctx context.Context,
	pre *prepared,
	tuning transportTuningConfig,
	measuredWidth, activeWidth, workLimit int,
	steady bool,
) (transportTuningConfig, error) {
	if tuning.parallelSet {
		tuning.connectionFit = connectionExplicit
		return finishPipelinePlan(pre, tuning, measuredWidth, activeWidth, workLimit)
	}
	tuning.connectionFit = connectionDefault
	if !steady || !tuning.fitSelected {
		return finishPipelinePlan(pre, tuning, measuredWidth, activeWidth, workLimit)
	}
	interval := effectiveBatchInterval(tuning, measuredWidth)
	if interval <= 0 {
		return finishPipelinePlan(pre, tuning, measuredWidth, activeWidth, workLimit)
	}
	origin, err := tth2.BatchOrigin(pre.allRequests())
	if err != nil {
		return tuning, err
	}
	base := pre.client.Transport
	probe := &tth2.Transport{
		TLSClientConfig:        base.TLSClientConfig,
		MaxResponseHeaderBytes: base.MaxResponseHeaderBytes,
		DialContext:            base.DialContext,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        base.IdleConnTimeout,
	}
	defer probe.CloseIdleConnections()
	var pathRTT time.Duration
	for range pathPingSamples {
		rtt, err := probe.Ping(ctx, origin)
		if err != nil {
			return tuning, fmt.Errorf("fit connection pipeline: %w", err)
		}
		tuning.pathRTTSamples = append(tuning.pathRTTSamples, rtt)
		pathRTT = max(pathRTT, rtt)
	}
	tuning.pathRTT = pathRTT
	tuning.connectionFit = connectionPathPing
	tuning.maxConns, err = fittedConnectionCount(pathRTT, interval)
	if err != nil {
		return tuning, err
	}
	tuning.pathFittedConns = tuning.maxConns
	return finishPipelinePlan(pre, tuning, measuredWidth, activeWidth, workLimit)
}

// finishPipelinePlan narrows a selected connection ceiling to the work a
// command can use, reviews unacknowledged automatic fan-out, and configures the
// trial transport from that effective count. measuredWidth sizes the fit;
// activeWidth includes active priming for stream review. workLimit counts
// independently distributable work units; zero means unbounded.
func finishPipelinePlan(
	pre *prepared,
	tuning transportTuningConfig,
	measuredWidth, activeWidth, workLimit int,
) (transportTuningConfig, error) {
	if measuredWidth < 1 || activeWidth < measuredWidth {
		return tuning, fmt.Errorf(
			"fit connection pipeline: batch widths must satisfy 1 <= measured <= active, got %d and %d",
			measuredWidth, activeWidth)
	}
	if workLimit < 0 {
		return tuning, fmt.Errorf(
			"fit connection pipeline: work limit must be >= 0, got %d",
			workLimit)
	}
	if tuning.connectionFit == connectionPathPing &&
		tuning.parallelFitSet && tuning.parallelFitMax > 0 &&
		tuning.maxConns > tuning.parallelFitMax {
		tuning.maxConns = tuning.parallelFitMax
		tuning.pathFitCapped = true
		tuning.connectionFit = connectionPathPingCapped
	}
	tuning.connectionLimit = tuning.maxConns
	if workLimit > 0 {
		tuning.connectionLimit = min(tuning.connectionLimit, workLimit)
	}
	activeStreams, ok := checkedProduct(tuning.connectionLimit, activeWidth)
	if !ok {
		return tuning, fmt.Errorf(
			"fit connection pipeline: active request stream count cannot be represented")
	}
	if tuning.connectionFit == connectionPathPing &&
		!tuning.parallelFitSet &&
		(tuning.connectionLimit > automaticFitReviewMaximumConnections ||
			activeStreams > automaticFitReviewMaximumStreams) {
		return tuning, fmt.Errorf(
			"automatic path fit resolves to %d connections and %d active "+
				"request streams for a batch of %d; review boundary is %d "+
				"connections and %d streams (PING RTT %s, effective interval "+
				"%s, fitted count %d); review local and target fan-out, then rerun "+
				"with --connections-fit-max %d",
			tuning.connectionLimit, activeStreams, activeWidth,
			automaticFitReviewMaximumConnections,
			automaticFitReviewMaximumStreams,
			compactDuration(tuning.pathRTT),
			compactDuration(effectiveBatchInterval(tuning, measuredWidth)),
			tuning.pathFittedConns, tuning.connectionLimit)
	}
	if pre != nil {
		pre.client.Transport.MaxConnsPerHost = tuning.connectionLimit
	}
	return tuning, nil
}

func effectiveBatchInterval(
	tuning transportTuningConfig,
	width int,
) time.Duration {
	interval := tuning.minBatchInterval
	if tuning.requestRateMax <= 0 || width <= 0 {
		return interval
	}
	requestInterval := time.Duration(math.Ceil(
		float64(width) * float64(time.Second) / tuning.requestRateMax))
	return max(interval, requestInterval)
}

func fittedConnectionCount(
	pathRTT, interval time.Duration,
) (int, error) {
	if interval <= 0 || pathRTT <= 0 {
		return 1, nil
	}
	base := pathRTT / interval
	if pathRTT%interval != 0 {
		base++
	}
	base = max(1, base)
	headroom := base / 4
	if base%4 != 0 {
		headroom++
	}
	headroom = max(1, headroom)
	if base > time.Duration(math.MaxInt)-headroom {
		return 0, fmt.Errorf(
			"fit connection pipeline: connection count with headroom cannot be represented")
	}
	return int(base + headroom), nil
}

func trialConnectionWorkLimit(
	trials, width int,
	arrangement tth2.ArrangementPolicy,
) int {
	if trials < 0 {
		return 0
	}
	if arrangement != tth2.ArrangeRotate {
		return trials
	}
	limit := trials / width
	if trials%width != 0 {
		limit++
	}
	return limit
}

// compactDuration retains about three significant digits. Durations used for
// delivery planning are noisy observations or human-selected intervals, so
// finer precision adds clutter without changing the resolved connection count.
// Go spells microseconds with a non-ASCII symbol; terminal output uses "us".
func compactDuration(d time.Duration) string {
	quantum := time.Nanosecond
	switch {
	case d >= 100*time.Second:
		quantum = time.Second
	case d >= 10*time.Second:
		quantum = 100 * time.Millisecond
	case d >= time.Second:
		quantum = 10 * time.Millisecond
	case d >= 100*time.Millisecond:
		quantum = time.Millisecond
	case d >= 10*time.Millisecond:
		quantum = 100 * time.Microsecond
	case d >= time.Millisecond:
		quantum = 10 * time.Microsecond
	case d >= 100*time.Microsecond:
		quantum = time.Microsecond
	case d >= 10*time.Microsecond:
		quantum = 100 * time.Nanosecond
	case d >= time.Microsecond:
		quantum = 10 * time.Nanosecond
	}
	rounded := d.Round(quantum)
	if rounded >= time.Microsecond && rounded < time.Millisecond {
		micros := float64(rounded) / float64(time.Microsecond)
		return strconv.FormatFloat(micros, 'f', -1, 64) + "us"
	}
	return rounded.String()
}
