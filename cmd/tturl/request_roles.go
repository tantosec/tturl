package main

import (
	"context"
	"fmt"

	"github.com/tantosec/tturl/internal/curlblocks"
)

// requestRoleFlags assigns expanded request blocks to a command's measured or
// distinct priming batch.
type requestRoleFlags struct {
	warmupOnly *curlblocks.Flag[bool]
}

type detectRequestRoleFlags struct {
	requestRoleFlags
	baselineOnly   *curlblocks.Flag[bool]
	baselineSupply *curlblocks.Flag[string]
}

type baselineSupply struct {
	count     int
	unlimited bool
}

func finiteBaselineSupply(count int) baselineSupply {
	return baselineSupply{count: count}
}

func unlimitedBaselineSupply() baselineSupply {
	return baselineSupply{unlimited: true}
}

func addRequestRoleFlags(parser *curlblocks.Parser) requestRoleFlags {
	return requestRoleFlags{
		warmupOnly: parser.Block.Bool("warmup-only", "", false,
			"block-only; use these requests for priming; requires --warmup > 0"),
	}
}

func addDetectRequestRoleFlags(parser *curlblocks.Parser) detectRequestRoleFlags {
	roles := parser.Block.Section("Detect block roles (after --block)").Notes(
		"Unlimited supply permits only one baseline block.")
	return detectRequestRoleFlags{
		requestRoleFlags: addRequestRoleFlags(parser),
		baselineOnly: roles.Bool(
			"baseline-only", "", false,
			"controls, not candidates; excludes --warmup-only; rejects --repeat, including inherited values"),
		baselineSupply: roles.String(
			"baseline-supply", "", "N|unlimited", "",
			"copies of each baseline request per comparison; requires --baseline-only").
			CompletionValues(unlimitedFlagValue).HelpDefault("1"),
	}
}

func (f requestRoleFlags) validate(plan *curlblocks.Plan, warmup int) error {
	hasDistinctWarmup := f.hasDistinctWarmup(plan)
	if hasDistinctWarmup && warmup == 0 {
		return fmt.Errorf("--warmup-only requires --warmup > 0")
	}
	return nil
}

func (f requestRoleFlags) hasDistinctWarmup(plan *curlblocks.Plan) bool {
	for _, block := range plan.Blocks {
		if f.warmupOnly.Get(block) {
			return true
		}
	}
	return false
}

func (f requestRoleFlags) assemble(
	ctx context.Context,
	plan *curlblocks.Plan,
) (*assembled, error) {
	return assembleWithRoles(ctx, plan, func(blockIndex int) bool {
		return f.warmupOnly.Get(plan.Blocks[blockIndex])
	})
}

func (f detectRequestRoleFlags) resolve(
	plan *curlblocks.Plan,
	warmup int,
) ([]requestGroupRole, []baselineSupply, error) {
	roles := make([]requestGroupRole, len(plan.Blocks))
	supplies := make([]baselineSupply, len(plan.Blocks))
	baselineBlocks := 0
	unlimited := false
	for i, block := range plan.Blocks {
		isWarmup := f.warmupOnly.Get(block)
		isBaseline := f.baselineOnly.Get(block)
		supplySet := f.baselineSupply.IsSet(block)
		switch {
		case isWarmup && isBaseline:
			return nil, nil, plan.BlockError(i, fmt.Errorf(
				"--baseline-only and --warmup-only are mutually exclusive"))
		case supplySet && !isBaseline:
			return nil, nil, plan.BlockError(i, fmt.Errorf(
				"--baseline-supply requires --baseline-only"))
		case isBaseline && block.RepeatIsSet():
			return nil, nil, plan.BlockError(i, fmt.Errorf(
				"--repeat cannot be used with --baseline-only; use "+
					"--baseline-supply N or --baseline-supply unlimited"))
		}

		if isWarmup {
			roles[i] = requestGroupWarmup
			continue
		}
		if !isBaseline {
			roles[i] = requestGroupCandidate
			continue
		}

		roles[i] = requestGroupBaseline
		baselineBlocks++
		supply := finiteBaselineSupply(1)
		if supplySet {
			value := f.baselineSupply.Get(block)
			count, err := parsePositiveIntOrUnlimited(value)
			if err != nil {
				return nil, nil, plan.BlockError(i, fmt.Errorf(
					"--baseline-supply %w", err))
			}
			if count < 0 {
				supply = unlimitedBaselineSupply()
			} else {
				supply = finiteBaselineSupply(count)
			}
		}
		supplies[i] = supply
		unlimited = unlimited || supply.unlimited
	}
	if unlimited && baselineBlocks > 1 {
		return nil, nil, fmt.Errorf(
			"an unlimited baseline block cannot be combined with another " +
				"--baseline-only block")
	}
	if err := f.validate(plan, warmup); err != nil {
		return nil, nil, err
	}
	return roles, supplies, nil
}

func (f detectRequestRoleFlags) assemble(
	ctx context.Context,
	plan *curlblocks.Plan,
	roles []requestGroupRole,
	supplies []baselineSupply,
) (*assembled, error) {
	return assembleWithRequestRoles(ctx, plan, func(blockIndex int) requestGroupRole {
		return roles[blockIndex]
	}, supplies)
}
