package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tantosec/tturl/tth2"
)

// driveAnalyse runs one fixed rotation experiment and feeds its unfiltered
// measured trials to the offline core. It resolves the bounded workload before
// opening a connection.
func driveAnalyse(
	ctx context.Context,
	client *tth2.Client,
	requests []*http.Request,
	cycles int,
	maxConns int,
	config analyseConfig,
	opts ...tth2.TrialsOption,
) analyseRunResult {
	width := len(requests)
	workload, workloadErr := resolveAnalyseWorkload(width, cycles)
	connectionLimit := 0
	var connectionErr error
	if workloadErr == nil {
		if maxConns < 1 {
			connectionErr = fmt.Errorf(
				"maximum connections must be >= 1, got %d", maxConns)
		} else {
			connectionLimit = min(maxConns, workload.Cycles)
		}
	}
	if client == nil || workloadErr != nil || connectionErr != nil {
		code := analyseInvalidWorkload
		detail := "fixed experiment has an invalid client or workload"
		if workloadErr != nil {
			detail = workloadErr.Error()
		}
		if connectionErr != nil {
			detail = connectionErr.Error()
		}
		if errors.Is(workloadErr, errAnalyseWorkloadTooLarge) {
			code = analyseEvidenceTooLarge
		}
		plannedCycles := cycles
		if workloadErr == nil {
			plannedCycles = workload.Cycles
		}
		return analyseOffline(analyseEvidence{
			Width: width,
			Execution: analyseExecutionEvidence{
				Recorded: true, PlannedCycles: plannedCycles,
			},
			Issue: &analyseEvidenceIssue{
				Code: code, Detail: detail,
			},
		}, config)
	}
	evidence, conns := collectAnalyseEvidenceConfigured(
		ctx, client, requests, workload, connectionLimit,
		config.WarmupWidth,
		config.Capture, config.observationSink, config.retainObservations,
		opts...,
	)
	config.ConnectionLimit = conns
	return finishAnalyseLive(evidence, conns, config)
}

func finishAnalyseLive(
	evidence analyseEvidence, conns int, config analyseConfig,
) analyseRunResult {
	if conns < 1 {
		evidence.Issue = &analyseEvidenceIssue{
			Code:   analyseInvalidWorkload,
			Detail: fmt.Sprintf("fixed experiment planned %d connections", conns),
		}
	}
	return analyseOffline(evidence, config)
}

func collectAnalyseEvidence(
	ctx context.Context,
	client *tth2.Client,
	requests []*http.Request,
	workload analyseWorkload,
	maxConns int,
	opts ...tth2.TrialsOption,
) (analyseEvidence, int) {
	return collectAnalyseEvidenceWithCapture(
		ctx, client, requests, workload, maxConns,
		raceCaptureConfig{}, opts...,
	)
}

func collectAnalyseEvidenceWithCapture(
	ctx context.Context,
	client *tth2.Client,
	requests []*http.Request,
	workload analyseWorkload,
	maxConns int,
	capture raceCaptureConfig,
	opts ...tth2.TrialsOption,
) (analyseEvidence, int) {
	return collectAnalyseEvidenceConfigured(
		ctx, client, requests, workload, maxConns, workload.Width,
		capture, nil, true,
		opts...)
}

func collectAnalyseEvidenceConfigured(
	ctx context.Context,
	client *tth2.Client,
	requests []*http.Request,
	workload analyseWorkload,
	maxConns int,
	warmupWidth int,
	capture raceCaptureConfig,
	sink func(analyseTrialObservation) error,
	retain bool,
	opts ...tth2.TrialsOption,
) (analyseEvidence, int) {
	opts = append(opts,
		tth2.WithArrangementPolicy(tth2.ArrangeRotate),
		tth2.WithMaxConns(maxConns),
		tth2.WithMaxTrials(workload.Trials),
	)
	opts = append(opts, capture.trialOptions()...)
	stream := client.StreamTrials(ctx, requests, opts...)
	collector := newLiveAnalyseCollector(
		workload.Width, workload.Cycles, warmupWidth, stream.Conns(),
		capture, sink, retain)
	for trial := range stream.All() {
		collector.observe(trial)
	}
	evidence := collector.finish(stream.Err())
	return evidence, stream.Conns()
}
