package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// deliveryPlan is the resolved offered-load and connection plan shared by text
// and structured reporting. Durations retain exact values here; only friendly
// presentation rounds them.
type deliveryPlan struct {
	batchRateMax           float64
	requestRateMax         float64
	effectiveBatchInterval time.Duration
	minBatchInterval       time.Duration
	connectionCeiling      int
	connectionLimit        int
	connectionSelection    connectionFit
	parallelFitMax         int
	parallelFitSet         bool
	pathRTTSamples         []time.Duration
	pathRTTBasis           time.Duration
	pathFittedConnections  int
	pathFitCapped          bool
	peerStreamLimitIgnored bool
	preview                bool
	pathFitUnresolved      bool
	pathFitInactive        bool
	activeBatchWidth       int
	connectionWorkLimit    int
}

// bodyReleasePlan separates configured release controls from whether the
// concrete request set can exercise them. Structured reports retain the
// configured values; friendly reports describe only their effective behaviour.
type bodyReleasePlan struct {
	releaseDelay         time.Duration
	bodyBytesWithheld    int
	hasBodyBytes         bool
	explicitlyConfigured bool
}

func (p bodyReleasePlan) delay() time.Duration {
	return p.releaseDelay
}

func resolveDeliveryPlan(
	tuning transportTuningConfig,
	width, connectionLimit int,
	peerStreamLimitIgnored bool,
) deliveryPlan {
	return deliveryPlan{
		pathFitInactive:        tuning.fitSelected && tuning.connectionFit == connectionDefault,
		batchRateMax:           tuning.batchRate,
		requestRateMax:         tuning.requestRateMax,
		effectiveBatchInterval: effectiveBatchInterval(tuning, width),
		minBatchInterval:       tuning.minBatchInterval,
		connectionCeiling:      tuning.maxConns,
		connectionLimit:        connectionLimit,
		connectionSelection:    tuning.connectionFit,
		parallelFitMax:         tuning.parallelFitMax,
		parallelFitSet:         tuning.parallelFitSet,
		pathRTTSamples:         append([]time.Duration(nil), tuning.pathRTTSamples...),
		pathRTTBasis:           tuning.pathRTT,
		pathFittedConnections:  tuning.pathFittedConns,
		pathFitCapped:          tuning.pathFitCapped,
		peerStreamLimitIgnored: peerStreamLimitIgnored,
	}
}

func previewDeliveryPlan(
	tuning transportTuningConfig,
	measuredWidth, activeWidth, workLimit int,
	steady bool,
	peerStreamLimitIgnored bool,
) (deliveryPlan, int, error) {
	needsPathFit := tuning.fitSelected && steady &&
		effectiveBatchInterval(tuning, measuredWidth) > 0
	if needsPathFit {
		return deliveryPlan{
			batchRateMax: tuning.batchRate, requestRateMax: tuning.requestRateMax,
			effectiveBatchInterval: effectiveBatchInterval(tuning, measuredWidth),
			minBatchInterval:       tuning.minBatchInterval,
			connectionSelection:    connectionPathPing,
			parallelFitMax:         tuning.parallelFitMax,
			parallelFitSet:         tuning.parallelFitSet,
			peerStreamLimitIgnored: peerStreamLimitIgnored,
			preview:                true,
			pathFitUnresolved:      true,
			activeBatchWidth:       activeWidth,
			connectionWorkLimit:    workLimit,
		}, 0, nil
	}
	if tuning.parallelSet {
		tuning.connectionFit = connectionExplicit
	} else {
		tuning.connectionFit = connectionDefault
	}
	var err error
	tuning, err = finishPipelinePlan(nil, tuning, measuredWidth, activeWidth, workLimit)
	if err != nil {
		return deliveryPlan{}, 0, err
	}
	plan := resolveDeliveryPlan(
		tuning, measuredWidth, tuning.connectionLimit, peerStreamLimitIgnored)
	plan.preview = true
	plan.activeBatchWidth = activeWidth
	plan.connectionWorkLimit = workLimit
	return plan, tuning.connectionLimit, nil
}

// reportDeliveryPlan presents the same resolved pacing and connection facts for
// every request command. A command may reduce the effective connection limit
// below the ceiling when its finite work cannot use every connection.
func reportDeliveryPlan(w io.Writer, plan deliveryPlan, width int) error {
	return reportDelivery(w, plan,
		fmt.Sprintf("%d requests/batch; ", width))
}

func reportDeliveryBatchWidths(
	w io.Writer,
	plan deliveryPlan,
	measuredWidth, warmupWidth int,
) error {
	var err error
	if warmupWidth == 0 || warmupWidth == measuredWidth {
		err = reportDeliveryPlan(w, plan, measuredWidth)
	} else {
		err = reportDelivery(w, plan, fmt.Sprintf(
			"%s/batch; %s/batch; ",
			countedNoun(measuredWidth, "measured request"),
			countedNoun(warmupWidth, "priming request")))
	}
	if err != nil {
		return err
	}
	return reportPrimingPacing(w, plan, measuredWidth, warmupWidth)
}

// reportDeliveryPolicy omits batch width when the command's experiment design
// has already stated it, while retaining the shared delivery vocabulary.
func reportDeliveryPolicy(w io.Writer, plan deliveryPlan) error {
	return reportDelivery(w, plan, "")
}

func reportDelivery(w io.Writer, plan deliveryPlan, batch string) error {
	batchRate := "unlimited"
	if plan.batchRateMax > 0 {
		batchRate = fmt.Sprintf("%g/s", plan.batchRateMax)
	}
	requestRate := "unlimited"
	if plan.requestRateMax > 0 {
		requestRate = fmt.Sprintf("%g/s", plan.requestRateMax)
	}
	interval := "unconstrained"
	if plan.effectiveBatchInterval > 0 {
		interval = compactDuration(plan.effectiveBatchInterval)
	}
	var output strings.Builder
	writeWrappedASCII(&output, "Pacing: ", fmt.Sprintf(
		"%sbatch-rate ceiling %s; request-rate ceiling %s; "+
			"minimum batch-start interval %s.",
		batch, batchRate, requestRate, interval))

	if plan.pathFitUnresolved {
		connections := "unresolved until execution; automatic HTTP/2 PING path fit required"
		switch {
		case plan.parallelFitSet && plan.parallelFitMax < 0:
			connections += "; '--connections-fit-max' is unlimited"
		case plan.parallelFitSet:
			connections += fmt.Sprintf(
				"; configured fit maximum %d", plan.parallelFitMax)
		}
		if plan.connectionWorkLimit > 0 {
			connections += fmt.Sprintf(
				"; finite work can use at most %d", plan.connectionWorkLimit)
		}

		writeWrappedASCII(&output, "Connections: ", connections+".")
		writeWrappedASCII(&output, "Active streams: ", fmt.Sprintf(
			"unresolved until execution; %s per active connection.",
			countedNoun(plan.activeBatchWidth, "request stream")))
		writeStreamPolicy(&output, plan.peerStreamLimitIgnored)
		_, err := io.WriteString(w, output.String())
		return err
	}

	selection := plan.connectionSelection
	if selection == "" {
		selection = connectionDefault
	}
	ceiling := max(plan.connectionCeiling, plan.connectionLimit)
	var connections string
	switch selection {
	case connectionDefault:
		connections = fmt.Sprintf(
			"up to %d active (default)", plan.connectionLimit)
	case connectionExplicit:
		if ceiling == plan.connectionLimit {
			connections = fmt.Sprintf(
				"up to %d active (configured by --connections)",
				plan.connectionLimit)
		} else {
			connections = fmt.Sprintf(
				"up to %d active for this run; configured maximum %d "+
					"(--connections)",
				plan.connectionLimit, ceiling)
		}
	case connectionPathPing, connectionPathPingCapped:
		fit := fmt.Sprintf(
			"from %s HTTP/2 PING RTT with 25%% (at least one connection) "+
				"headroom; network RTT only; service time excluded",
			compactDuration(plan.pathRTTBasis))
		if ceiling == plan.connectionLimit {
			connections = fmt.Sprintf(
				"up to %d active; path-fitted %s",
				plan.connectionLimit, fit)
		} else {
			connections = fmt.Sprintf(
				"up to %d active for this run; path-fitted ceiling %d %s",
				plan.connectionLimit, ceiling, fit)
		}
		switch {
		case plan.pathFitCapped:
			connections += fmt.Sprintf(
				"; capped at %d by --connections-fit-max",
				plan.parallelFitMax)
		case plan.parallelFitSet && plan.parallelFitMax == -1:
			connections += "; --connections-fit-max unlimited permits any fit"
		case plan.parallelFitSet:
			connections += fmt.Sprintf(
				"; --connections-fit-max %d", plan.parallelFitMax)
		}
	}
	if plan.pathFitInactive {
		connections += "; requested fitting inactive for the single-trial workload"
		if plan.parallelFitSet {
			connections += "; configured fit maximum inactive"
		}
	}
	writeWrappedASCII(&output, "Connections: ", connections+".")
	if plan.preview || plan.activeBatchWidth > 0 {
		activeStreams, ok := checkedProduct(
			plan.connectionLimit, plan.activeBatchWidth)
		if !ok {
			return fmt.Errorf("active request stream count cannot be represented")
		}
		writeWrappedASCII(&output, "Active streams: ", fmt.Sprintf(
			"up to %d (%s x %s/connection).", activeStreams,
			countedNoun(plan.connectionLimit, "connection"),
			countedNoun(plan.activeBatchWidth, "request stream")))
	}
	writeStreamPolicy(&output, plan.peerStreamLimitIgnored)
	if plan.pathFitCapped {
		writeWrappedASCII(&output, "Caution: ", fmt.Sprintf(
			"path fit requested %d connections; the %d-connection fit "+
				"maximum may make the requested batch cadence unsustainable.",
			plan.pathFittedConnections, plan.parallelFitMax))
	}
	_, err := io.WriteString(w, output.String())
	return err
}

func writeStreamPolicy(w io.Writer, ignored bool) {
	policy := "honour the peer's advertised concurrent-stream limit"
	if ignored {
		policy = "intentionally ignore the peer's advertised concurrent-stream limit"
	}
	writeWrappedASCII(w, "Streams: ", policy+".")
}

// reportReleasePlan presents the final body-release configuration shared
// by request commands without requiring each command to reproduce its wording.
func reportReleasePlan(w io.Writer, plan bodyReleasePlan) {
	switch {
	case !plan.hasBodyBytes && !plan.explicitlyConfigured:
		return
	case !plan.hasBodyBytes:
		writeWrappedASCII(w, "Body release: ",
			"not applicable; requests contain no body bytes.")
	case plan.bodyBytesWithheld == 0:
		writeWrappedASCII(w, "Body release: ",
			"no fixed tail or delay; non-empty bodies share a final record.")
	case plan.releaseDelay == 0:
		writeWrappedASCII(w, "Body release: ", fmt.Sprintf(
			"withhold up to %s per body; no release delay.",
			countedNoun(plan.bodyBytesWithheld, "trailing byte")))
	default:
		writeWrappedASCII(w, "Body release: ", fmt.Sprintf(
			"withhold up to %s per body; delay release by %s.",
			countedNoun(plan.bodyBytesWithheld, "trailing byte"),
			formatDurationASCII(plan.releaseDelay)))
	}
}

// reportTLSPlan records the non-default TLS policy shared by request commands.
// Successful default verification needs no report line.
func reportTLSPlan(w io.Writer, insecure bool) {
	if insecure {
		emitln(w, "TLS: certificate verification disabled (--insecure).")
	}
}

// reportPrimingPacing separates connection sizing from wider priming pacing.
func reportPrimingPacing(w io.Writer, plan deliveryPlan, measuredWidth, warmupWidth int) error {
	if warmupWidth <= measuredWidth {
		return nil
	}
	output := newReportWriter(w)
	if plan.connectionSelection == connectionPathPing || plan.connectionSelection == connectionPathPingCapped {
		writeWrappedASCII(output, "Fit interval: ", fmt.Sprintf(
			"%s for %s; connections fitted for measurement.",
			compactDuration(plan.effectiveBatchInterval), countedNoun(measuredWidth, "measured request")))
	}
	interval := plan.intervalForWidth(warmupWidth)
	if interval > 0 {
		writeWrappedASCII(output, "Priming pacing: ", fmt.Sprintf(
			"%s per batch; minimum batch-start interval %s.",
			countedNoun(warmupWidth, "request"), compactDuration(interval)))
	}
	return output.Err()
}

func (plan deliveryPlan) intervalForWidth(width int) time.Duration {
	return effectiveBatchInterval(transportTuningConfig{
		minBatchInterval: plan.minBatchInterval, requestRateMax: plan.requestRateMax,
	}, width)
}
