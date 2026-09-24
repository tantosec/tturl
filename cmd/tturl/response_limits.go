package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

const (
	defaultResponseBodyMax = int64(8 << 20)
	defaultBatchTimeout    = 30 * time.Second
)

type responseLimitFlags struct {
	bodyMax      *curlblocks.Flag[string]
	batchTimeout *curlblocks.Flag[string]
}

type responseLimits struct {
	maxBodyBytes int64
	batchTimeout time.Duration
}

func defaultResponseLimits() responseLimits {
	return responseLimits{
		maxBodyBytes: defaultResponseBodyMax,
		batchTimeout: defaultBatchTimeout,
	}
}

func addResponseLimitFlags(r *curlblocks.Registry) *responseLimitFlags {
	r.Notes("SIZE is bytes, optionally suffixed B, KiB, MiB or GiB.")
	return &responseLimitFlags{
		bodyMax: r.String(
			"response-body-max", "", "SIZE|unlimited",
			strconv.FormatInt(defaultResponseBodyMax/(1<<20), 10)+"MiB",
			"limit accepted response body").
			CompletionValues(unlimitedFlagValue),
		batchTimeout: r.String(
			"batch-timeout", "", "DURATION|unlimited",
			defaultBatchTimeout.String(),
			"limit each transport attempt; finite value must exceed configured release-delay").
			CompletionValues(unlimitedFlagValue),
	}
}

func (f *responseLimitFlags) resolve(
	globals curlblocks.Globals,
	release bodyReleasePlan,
) (responseLimits, error) {
	bodyMax, err := parsePositiveByteSizeOrUnlimited(f.bodyMax.Get(globals))
	if err != nil {
		return responseLimits{}, fmt.Errorf("--response-body-max %w", err)
	}
	timeout, unlimited, err := parseDurationOrUnlimited(
		f.batchTimeout.Get(globals))
	if err != nil || (!unlimited && timeout <= 0) {
		return responseLimits{}, fmt.Errorf(
			"--batch-timeout must be a positive Go duration or unlimited")
	}
	if unlimited {
		timeout = 0
	}
	if timeout > 0 && timeout <= release.delay() {
		return responseLimits{}, fmt.Errorf(
			"--batch-timeout must exceed --release-delay (%s)",
			formatDurationASCII(release.delay()))
	}
	return responseLimits{
		maxBodyBytes: bodyMax,
		batchTimeout: timeout,
	}, nil
}

func parsePositiveByteSizeOrUnlimited(value string) (int64, error) {
	if isUnlimitedFlagValue(value) {
		return 0, nil
	}
	lower := strings.ToLower(value)
	number := value
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{
		{suffix: "gib", scale: 1 << 30},
		{suffix: "mib", scale: 1 << 20},
		{suffix: "kib", scale: 1 << 10},
		{suffix: "b", scale: 1},
	} {
		if strings.HasSuffix(lower, unit.suffix) {
			number = value[:len(value)-len(unit.suffix)]
			multiplier = unit.scale
			break
		}
	}
	if number == "" {
		return 0, responseBodyMaxValueError(value)
	}
	for _, digit := range []byte(number) {
		if digit < '0' || digit > '9' {
			return 0, responseBodyMaxValueError(value)
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 1 || n > math.MaxInt64/multiplier {
		return 0, responseBodyMaxValueError(value)
	}
	return n * multiplier, nil
}

func responseBodyMaxValueError(value string) error {
	return fmt.Errorf(
		"must be positive bytes with an optional B, KiB, MiB, or GiB "+
			"suffix, or unlimited; got %q",
		curlblocks.DisplayText(value))
}

func (l responseLimits) trialOptions() []tth2.TrialsOption {
	return []tth2.TrialsOption{
		tth2.WithMaxResponseBodyBytes(l.maxBodyBytes),
		tth2.WithBatchTimeout(l.batchTimeout),
	}
}

func (l responseLimits) sendOptions() []tth2.SendOption {
	return []tth2.SendOption{
		tth2.WithMaxResponseBodyBytes(l.maxBodyBytes),
		tth2.WithBatchTimeout(l.batchTimeout),
	}
}

func reportResponseLimits(w io.Writer, l responseLimits) {
	body := "unlimited"
	if l.maxBodyBytes > 0 {
		body = formatResponseBodyMax(l.maxBodyBytes)
	}
	timeout := "unlimited"
	if l.batchTimeout > 0 {
		timeout = formatDurationASCII(l.batchTimeout)
	}
	writeWrappedASCII(w, "Response limits: ", fmt.Sprintf(
		"%s accepted body per stream; %s transport time per batch.",
		body, timeout))
}

func formatResponseBodyMax(n int64) string {
	for _, unit := range []struct {
		name  string
		scale int64
	}{
		{name: "GiB", scale: 1 << 30},
		{name: "MiB", scale: 1 << 20},
		{name: "KiB", scale: 1 << 10},
	} {
		if n >= unit.scale && n%unit.scale == 0 {
			return fmt.Sprintf("%d%s", n/unit.scale, unit.name)
		}
	}
	return fmt.Sprintf("%dB", n)
}

type batchFailureCode string

const (
	failureResponseBodyLimit batchFailureCode = "response_body_limit"
	failureBatchTimeout      batchFailureCode = "batch_timeout"
)

type batchFailure struct {
	Code   batchFailureCode
	Detail string
}

func classifyBatchFailure(err error) *batchFailure {
	if bodyLimit, ok := errors.AsType[*tth2.ResponseBodyLimitError](err); ok {
		return &batchFailure{
			Code: failureResponseBodyLimit, Detail: bodyLimit.Error(),
		}
	}
	if timeout, ok := errors.AsType[*tth2.BatchTimeoutError](err); ok {
		return &batchFailure{
			Code: failureBatchTimeout, Detail: timeout.Error(),
		}
	}
	return nil
}
