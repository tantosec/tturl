package curlblocks

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseRate converts an "N/UNIT" rate or "unlimited" to a per-second value.
// UNIT is s/m/h/d and is required; N is a positive integer. The grammar rejects
// fractional, signed, exponent, and non-finite forms. The empty string means
// "unset"; both unset and unlimited are reported as 0.
func ParseRate(spec string) (float64, error) {
	if spec == "" || spec == "unlimited" {
		return 0, nil
	}
	num, unit, hasUnit := strings.Cut(spec, "/")
	n, ok := parsePositiveInt(num)
	if !ok {
		return 0, fmt.Errorf("count must be a positive integer, got %q", num)
	}
	if !hasUnit {
		return 0, fmt.Errorf("unit is required; use %s/s, %s/m, %s/h, or %s/d",
			num, num, num, num)
	}
	var div float64
	switch unit {
	case "s":
		div = 1
	case "m":
		div = 60
	case "h":
		div = 3600
	case "d":
		div = 86400
	default:
		return 0, fmt.Errorf("unit must be s, m, h, or d, got %q", unit)
	}
	return float64(n) / div, nil
}

// parsePositiveInt parses a plain positive integer: ASCII digits only, so no
// sign, decimal point, or exponent, and no leading or trailing space. That is
// curl's grammar for a count, and being this strict is also what keeps NaN and
// Inf out. A float parse accepts both, and NaN then slips past a range check
// because every comparison against it is false.
func parsePositiveInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s) // digits only by here, so this catches overflow
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// Default help text for the pacing flags, generic so it reads for any
// consumer; a tool whose semantics warrant sharper wording passes its own to
// Parser.PacingFlags.
const (
	defaultParallelMaxUsage = "limit concurrent requests"
	defaultBatchRateUsage   = "cap batches; UNIT=s,m,h,d"
	defaultRequestRateUsage = "cap requests; UNIT=s,m,h,d"
)

// PacingFlags is the resolved connection, batch-rate, and request-rate group a
// [Parser.PacingFlags] call registers.
type PacingFlags struct {
	parallelMax    *Flag[int]
	batchRateMax   *Flag[string]
	requestRateMax *Flag[string]
}

// PacingFlags registers the supplied connection flag, --batch-rate-max, and
// --request-rate-max and returns typed accessors.
// Rate values use N/UNIT with an explicit s, m, h, or d unit, or unlimited.
// Concurrency defaults to one; rates default to unlimited.
//
// r and rates may be [Parser.Global] or its section handles. r owns the
// connection flag; rates owns both rate flags. Other scopes panic. Empty usage
// strings select the package defaults.
func (p *Parser) PacingFlags(
	r, rates *Registry,
	parallelMaxName, parallelMaxUsage, batchRateUsage, requestRateUsage string,
) *PacingFlags {
	if r == nil || r.shared.scope != scopeGlobal ||
		rates == nil || rates.shared.scope != scopeGlobal {
		panic("curlblocks: PacingFlags needs global-scope registries (Parser.Global, or one of its " +
			"Section handles): pacing flags are read back from Globals")
	}
	if parallelMaxUsage == "" {
		parallelMaxUsage = defaultParallelMaxUsage
	}
	if batchRateUsage == "" {
		batchRateUsage = defaultBatchRateUsage
	}
	if requestRateUsage == "" {
		requestRateUsage = defaultRequestRateUsage
	}
	p.pacing = &PacingFlags{
		parallelMax: r.Int(parallelMaxName, "", "N", 1, parallelMaxUsage),
		batchRateMax: rates.String("batch-rate-max", "", "N/UNIT|unlimited", "", batchRateUsage).
			HelpDefault("unlimited").CompletionValues("unlimited"),
		requestRateMax: rates.String(
			"request-rate-max", "", "N/UNIT|unlimited", "", requestRateUsage).
			HelpDefault("unlimited").CompletionValues("unlimited"),
	}
	return p.pacing
}

// ParallelMaxHelpDefault replaces the connection flag's displayed default.
// It does not change the parsed default. A consumer can use it when omission
// activates a higher-level policy such as automatic connection fitting.
func (pf *PacingFlags) ParallelMaxHelpDefault(display string) *PacingFlags {
	pf.parallelMax.HelpDefault(display)
	return pf
}

// validate checks both flags' values, so a bad one is reported by Parse along
// with every other command-line fault rather than at the consumer's read. A nil
// receiver is a parser with no pacing flags registered, which has nothing to
// check.
func (pf *PacingFlags) validate(g Globals) error {
	if pf == nil {
		return nil
	}
	if _, err := pf.ParallelMax(g); err != nil {
		return err
	}
	if _, err := pf.BatchRateMax(g); err != nil {
		return err
	}
	_, err := pf.RequestRateMax(g)
	return err
}

// ParallelMax resolves the connection flag, the concurrency capacity.
// It must be >= 1, which Parse has already checked.
func (pf *PacingFlags) ParallelMax(g Globals) (int, error) {
	n := pf.parallelMax.Get(g)
	if n < 1 {
		return 0, fmt.Errorf("--%s must be >= 1, got %d", pf.parallelMax.nm, n)
	}
	return n, nil
}

// ParallelMaxIsSet reports whether the connection flag was supplied explicitly.
func (pf *PacingFlags) ParallelMaxIsSet(g Globals) bool {
	return pf.parallelMax.IsSet(g)
}

// BatchRateMax resolves --batch-rate-max to batches per second.
// 0 means no limit.
func (pf *PacingFlags) BatchRateMax(g Globals) (float64, error) {
	return parseNamedRate("batch-rate-max", pf.batchRateMax.Get(g))
}

// RequestRateMax resolves --request-rate-max to requests per second. 0 means
// no limit.
func (pf *PacingFlags) RequestRateMax(g Globals) (float64, error) {
	return parseNamedRate("request-rate-max", pf.requestRateMax.Get(g))
}

func parseNamedRate(name, spec string) (float64, error) {
	rate, err := ParseRate(spec)
	if err != nil {
		return 0, fmt.Errorf("--%s: %w", name, err)
	}
	return rate, nil
}
