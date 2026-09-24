package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

type detectFailurePhase int

const (
	detectFailurePriming detectFailurePhase = iota
	detectFailureInitial
	detectFailureReplacement
	detectFailureComparison
)

type detectFailureKey struct {
	phase  detectFailurePhase
	code   string
	detail string
}

type detectFailureIdentity struct {
	worker     int
	connection tth2.ConnectionID
}

type detectFailureGroup struct {
	key            detectFailureKey
	attempts       int
	identities     map[detectFailureIdentity]struct{}
	participations int
	representative string
}

// textFailureDetail preserves semantic causes while omitting per-attempt data.
func textFailureDetail(err error, code string) string {
	switch code {
	case "response_body_limit", "batch_timeout", "run_timeout", "interrupted",
		"deadline_exceeded":
		return ""
	}
	if operation, ok := errors.AsType[*net.OpError](err); ok {
		return fmt.Sprintf("%q %q %s", operation.Op, operation.Net,
			textFailureDetail(operation.Err, ""))
	}
	if connection, ok := errors.AsType[http2.ConnectionError](err); ok {
		return fmt.Sprintf("http2 connection %d", connection)
	}
	if stream, ok := errors.AsType[*tth2.StreamError](err); ok {
		return fmt.Sprintf("http2 stream %d", stream.Code)
	}
	if protocolStream, ok := errors.AsType[http2.StreamError](err); ok {
		return fmt.Sprintf("http2 stream %d", protocolStream.Code)
	}

	for errors.Unwrap(err) != nil {
		err = errors.Unwrap(err)
	}
	return fmt.Sprintf("%T: %s", err, err.Error())
}

func detectTextFailureGroups(evidence detectExecutionEvidence) []detectFailureGroup {
	groups := make(map[detectFailureKey]*detectFailureGroup)
	add := func(phase detectFailurePhase, identity detectFailureIdentity, operations int,
		err error, fallback string,
	) {
		failure := makeStructuredFailure(err, fallback)
		if failure == nil {
			return
		}
		key := detectFailureKey{phase, failure.Code, textFailureDetail(err, failure.Code)}
		group := groups[key]
		if group == nil {
			group = &detectFailureGroup{
				key: key, identities: make(map[detectFailureIdentity]struct{}),
				representative: err.Error(),
			}
			groups[key] = group
		}
		group.attempts++
		if phase == detectFailureInitial || phase == detectFailureReplacement || identity.connection != 0 {
			group.identities[identity] = struct{}{}
		}
		group.participations += operations
		group.representative = min(group.representative, err.Error())
	}
	for _, attempt := range evidence.PrimingAttempts {
		if !attempt.Complete {
			add(detectFailurePriming, detectFailureIdentity{connection: attempt.Connection}, 0,
				attempt.Err, "acquisition_failed")
		}
	}
	for _, failure := range evidence.AcquisitionFailures {
		phase := detectFailureInitial
		if failure.Replacement {
			phase = detectFailureReplacement
		}
		add(phase, detectFailureIdentity{worker: failure.Worker}, 0, failure.Err, "acquisition_failed")
	}
	for _, failure := range evidence.ComparisonFailures {
		add(detectFailureComparison, detectFailureIdentity{connection: failure.Connection},
			failure.Participations, failure.Err, "comparison_failed")
	}
	result := make([]detectFailureGroup, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}
	slices.SortFunc(result, func(a, b detectFailureGroup) int {
		if order := cmp.Compare(a.key.phase, b.key.phase); order != 0 {
			return order
		}
		if order := cmp.Compare(a.key.code, b.key.code); order != 0 {
			return order
		}
		return cmp.Compare(a.key.detail, b.key.detail)
	})
	return result
}

func reportDetectFailureGroups(w io.Writer, result detectRunResult) {
	headings := [...]string{
		"Priming", "Initial acquisition",
		"Replacement acquisition", "Comparison",
	}
	nouns := [...]string{
		"incomplete batch", "failed attempt", "failed attempt",
		"incomplete comparison",
	}
	populations := [...]string{"connection", "worker", "worker", "connection"}
	groups := detectTextFailureGroups(result.Execution)
	stopped := normaliseCompletion(result.Completion).State == completionStopped
	if stopped {
		// Keep ordinary causes in their sorted order, then show cancellations.
		slices.SortStableFunc(groups, func(a, b detectFailureGroup) int {
			if order := cmp.Compare(a.key.phase, b.key.phase); order != 0 {
				return order
			}
			if (a.key.code == "interrupted") != (b.key.code == "interrupted") {
				if a.key.code == "interrupted" {
					return 1
				}
				return -1
			}
			return 0
		})
	}
	previous := detectFailurePhase(-1)
	for _, group := range groups {
		phase := group.key.phase
		counts := fmt.Sprintf("%s across %s", countedNoun(group.attempts, nouns[phase]),
			countedNoun(len(group.identities), populations[phase]))
		if phase == detectFailureComparison {
			counts += fmt.Sprintf("; %s", countedNoun(group.participations,
				"attempted request participation"))
		}
		if stopped && group.key.code == "interrupted" {
			writeWrappedASCII(w, headings[phase]+" cancellations: ", counts+".")
			continue
		}
		if phase != previous {
			emitln(w, headings[phase]+" failures:")
			previous = phase
		}
		writeWrappedASCII(w, "  "+group.key.code+": ", counts+".")
		writeWrappedASCII(w, "    Cause: ", curlblocks.DisplayText(group.representative))
	}
}
