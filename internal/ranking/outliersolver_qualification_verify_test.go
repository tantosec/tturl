package ranking

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	qualificationSummarySchema = "tturl/ranking/qualification-summary/v2"
	maxEvidenceLineBytes       = 1 << 20
	maxEvidenceFiles           = 64
)

var (
	qualificationVerifyPathFlag = flag.String(
		"qualification-verify.path", "",
		"qualification JSONL file or directory to verify",
	)
	qualificationVerifySummaryFlag = flag.String(
		"qualification-verify.summary", "",
		"new path for the machine-readable verification summary",
	)
	qualificationVerifyDispositionFlag = flag.String(
		"qualification-verify.disposition", "",
		"new path for a qualification disposition template",
	)
	qualificationReplayPathFlag = flag.String(
		"qualification-replay.path", "",
		"qualification JSONL file containing the record to replay",
	)
	qualificationReplayScenarioFlag = flag.String(
		"qualification-replay.scenario", "", "exact scenario ID to replay",
	)
	qualificationReplaySolverFlag = flag.String(
		"qualification-replay.solver", "", "exact solver ID to replay",
	)
	qualificationReplaySeedFlag = flag.String(
		"qualification-replay.seed", "", "exact master seed to replay",
	)
)

type qualificationCellSummary struct {
	Scenario        string                 `json:"scenario"`
	Solver          string                 `json:"solver"`
	Check           qualificationCheckKind `json:"check"`
	Errors          int                    `json:"errors"`
	Runs            int                    `json:"runs"`
	CriticalErrors  int                    `json:"critical_errors"`
	NominalBudget   float64                `json:"nominal_budget,omitempty"`
	RegressionRate  float64                `json:"regression_rate,omitempty"`
	FalseAcceptRate float64                `json:"false_accept_rate,omitempty"`
	FalseRejectRate float64                `json:"false_reject_rate,omitempty"`
	Passed          bool                   `json:"passed"`
}

type qualificationFailureKind string

const (
	qualificationDeterministicFailure qualificationFailureKind = "deterministic"
	qualificationSampledRejection     qualificationFailureKind = "sampled-rejection"
	qualificationExactRejection       qualificationFailureKind = "exact-rejection"
)

type qualificationFailureSummary struct {
	Kind           qualificationFailureKind     `json:"kind"`
	Scenario       string                       `json:"scenario"`
	Solver         string                       `json:"solver"`
	Check          qualificationCheckKind       `json:"check,omitempty"`
	Errors         int                          `json:"errors"`
	CriticalErrors int                          `json:"critical_errors"`
	Records        []qualificationFailureRecord `json:"records"`
}

type qualificationSummary struct {
	Schema        string                        `json:"schema"`
	PlanID        string                        `json:"plan_id"`
	Campaign      qualificationCampaignKind     `json:"campaign"`
	SourceSHA256  string                        `json:"source_sha256"`
	Revision      string                        `json:"revision,omitempty"`
	SeedStart     uint64                        `json:"seed_start"`
	SeedEnd       uint64                        `json:"seed_end"`
	Runs          int                           `json:"runs"`
	Fleet         []string                      `json:"fleet"`
	EvidenceFiles int                           `json:"evidence_files"`
	Records       int                           `json:"records"`
	Cells         []qualificationCellSummary    `json:"cells"`
	Failures      []qualificationFailureSummary `json:"failures,omitempty"`
	Violations    []string                      `json:"violations,omitempty"`
	Result        string                        `json:"result"`
}

type qualificationEvidenceFile struct {
	Path       string
	Manifest   qualificationManifest
	Completion evidenceCompletion
}

func readEvidenceLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("evidence line exceeds %d bytes", maxEvidenceLineBytes)
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) != 0 {
			return nil, fmt.Errorf("unterminated evidence line")
		}
		return nil, err
	}
	return slices.Clone(line), nil
}

func decodeCanonicalEvidenceLine(line []byte, value any) error {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return fmt.Errorf("evidence line is not newline terminated")
	}
	decoder := json.NewDecoder(bytes.NewReader(line[:len(line)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("evidence line contains trailing JSON")
		}
		return err
	}
	canonical, err := canonicalJSONLine(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(line, canonical) {
		return fmt.Errorf("evidence line is not canonical JSON")
	}
	return nil
}

func decodeLegacyCanonicalEvidenceLine(line []byte, value any) error {
	updated := bytes.ReplaceAll(line,
		// cspell:disable-next-line
		[]byte(`"quantization":`), []byte(`"quantisation":`))
	return decodeCanonicalEvidenceLine(updated, value)
}

func TestQualificationEvidenceSchemaSpellingCompatibility(t *testing.T) {
	want := evaluationRunRecord{
		Schema: evaluationRunSchema,
		Channel: evaluationChannelSpec{Regime: &evaluationRegimeSpec{
			BaseNoiseScale: 1, Quantisation: 2,
		}},
	}
	current, err := canonicalJSONLine(want)
	if err != nil {
		t.Fatal(err)
	}
	var currentRecord evaluationRunRecord
	if err := decodeCanonicalEvidenceLine(current, &currentRecord); err != nil {
		t.Fatalf("decode current record: %v", err)
	}
	if !reflect.DeepEqual(currentRecord, want) {
		t.Fatalf("current record = %+v, want %+v", currentRecord, want)
	}

	legacy := bytes.ReplaceAll(current,
		[]byte(evaluationRunSchema), []byte(evaluationLegacyRunSchema))
	legacy = bytes.ReplaceAll(legacy,
		// cspell:disable-next-line
		[]byte(`"quantisation":`), []byte(`"quantization":`))
	var legacyRecord evaluationRunRecord
	if err := decodeLegacyCanonicalEvidenceLine(legacy, &legacyRecord); err != nil {
		t.Fatalf("decode retained record: %v", err)
	}
	want.Schema = evaluationLegacyRunSchema
	if !reflect.DeepEqual(legacyRecord, want) {
		t.Fatalf("retained record = %+v, want %+v", legacyRecord, want)
	}

	wantManifest := qualificationManifest{
		Schema: qualificationMetaSchema,
		Scenarios: []evaluationScenarioManifest{{
			Name: "quantised", Channel: evaluationChannelSpec{
				Regime: &evaluationRegimeSpec{
					BaseNoiseScale: 1, Quantisation: 2,
				},
			},
		}},
	}
	currentManifest, err := canonicalJSONLine(wantManifest)
	if err != nil {
		t.Fatal(err)
	}
	legacyManifest := bytes.ReplaceAll(currentManifest,
		[]byte(qualificationMetaSchema), []byte(qualificationLegacyMetaSchema))
	legacyManifest = bytes.ReplaceAll(legacyManifest,
		// cspell:disable-next-line
		[]byte(`"quantisation":`), []byte(`"quantization":`))
	var decodedManifest qualificationManifest
	if err := decodeLegacyCanonicalEvidenceLine(
		legacyManifest, &decodedManifest); err != nil {
		t.Fatalf("decode retained manifest: %v", err)
	}
	wantManifest.Schema = qualificationLegacyMetaSchema
	if !reflect.DeepEqual(decodedManifest, wantManifest) {
		t.Fatalf("retained manifest = %+v, want %+v", decodedManifest, wantManifest)
	}
}

func readQualificationEvidence(
	path string,
	onManifest func(qualificationManifest) error,
	onRecord func(qualificationManifest, evaluationRunRecord) error,
) (qualificationEvidenceFile, error) {
	file, err := os.Open(path) //nolint:gosec // Explicit evidence input path.
	if err != nil {
		return qualificationEvidenceFile{}, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, maxEvidenceLineBytes)
	manifestLine, err := readEvidenceLine(reader)
	if err != nil {
		return qualificationEvidenceFile{}, fmt.Errorf("read manifest: %w", err)
	}
	var manifestEnvelope struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(manifestLine, &manifestEnvelope); err != nil {
		return qualificationEvidenceFile{}, fmt.Errorf("decode manifest schema: %w", err)
	}
	var manifest qualificationManifest
	decodeManifest := decodeCanonicalEvidenceLine
	if manifestEnvelope.Schema == qualificationLegacyMetaSchema {
		decodeManifest = decodeLegacyCanonicalEvidenceLine
	}
	if err := decodeManifest(manifestLine, &manifest); err != nil {
		return qualificationEvidenceFile{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := onManifest(manifest); err != nil {
		return qualificationEvidenceFile{}, err
	}
	manifestDigest := sha256.Sum256(manifestLine)
	recordHash := sha256.New()
	records := 0
	var completion evidenceCompletion
	for {
		line, err := readEvidenceLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return qualificationEvidenceFile{}, fmt.Errorf("missing completion record")
			}
			return qualificationEvidenceFile{}, err
		}
		var envelope struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return qualificationEvidenceFile{}, fmt.Errorf("decode evidence schema: %w", err)
		}
		if envelope.Schema == evidenceCompletionSchema {
			if err := decodeCanonicalEvidenceLine(line, &completion); err != nil {
				return qualificationEvidenceFile{}, fmt.Errorf("decode completion: %w", err)
			}
			break
		}
		expectedRecords := manifest.Runs * len(manifest.Scenarios) * len(manifest.Shard)
		if records >= expectedRecords {
			return qualificationEvidenceFile{}, fmt.Errorf(
				"evidence contains more than %d planned records", expectedRecords)
		}
		var record evaluationRunRecord
		decodeRecord := decodeCanonicalEvidenceLine
		if envelope.Schema == evaluationLegacyRunSchema {
			decodeRecord = decodeLegacyCanonicalEvidenceLine
		}
		if err := decodeRecord(line, &record); err != nil {
			return qualificationEvidenceFile{}, fmt.Errorf("decode record %d: %w",
				records, err)
		}
		if err := onRecord(manifest, record); err != nil {
			return qualificationEvidenceFile{}, fmt.Errorf("record %d: %w", records, err)
		}
		_, _ = recordHash.Write(line)
		records++
	}
	if completion.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return qualificationEvidenceFile{}, fmt.Errorf("manifest digest mismatch")
	}
	if completion.RecordsSHA256 != hex.EncodeToString(recordHash.Sum(nil)) {
		return qualificationEvidenceFile{}, fmt.Errorf("record digest mismatch")
	}
	if completion.Records != records {
		return qualificationEvidenceFile{}, fmt.Errorf(
			"completion records = %d, observed %d", completion.Records, records)
	}
	if line, err := readEvidenceLine(reader); err == nil {
		return qualificationEvidenceFile{}, fmt.Errorf(
			"data follows completion record: %q", strings.TrimSpace(string(line)))
	} else if !errors.Is(err, io.EOF) {
		return qualificationEvidenceFile{}, err
	}
	return qualificationEvidenceFile{
		Path: path, Manifest: manifest, Completion: completion,
	}, nil
}

func qualificationEvidencePaths(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{root}, nil
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
			paths = append(paths, path)
			if len(paths) > maxEvidenceFiles {
				return fmt.Errorf("more than %d evidence files", maxEvidenceFiles)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no JSONL evidence files under %q", root)
	}
	return paths, nil
}

func qualificationManifestSolvers(
	plan qualificationPlan, manifest qualificationManifest,
) ([]evaluationScenario, []evaluationSolver, error) {
	if len(manifest.Shard) == 0 {
		return nil, nil, fmt.Errorf("manifest shard is empty")
	}
	if len(manifest.Shard) != 1 && !slices.Equal(manifest.Shard, plan.Fleet) {
		return nil, nil, fmt.Errorf("manifest shard must contain one solver or the fleet")
	}
	allScenarios, allSolvers, err := qualificationInputs(plan, "all")
	if err != nil {
		return nil, nil, err
	}
	solverByName := make(map[string]evaluationSolver, len(allSolvers))
	for _, solver := range allSolvers {
		solverByName[solver.Name] = solver
	}
	selected := make([]evaluationSolver, 0, len(manifest.Shard))
	for _, name := range manifest.Shard {
		solver, ok := solverByName[name]
		if !ok {
			return nil, nil, fmt.Errorf("manifest shard solver %q is not in the fleet", name)
		}
		selected = append(selected, solver)
	}
	return allScenarios, selected, nil
}

func validateQualificationManifest(
	plan qualificationPlan, manifest qualificationManifest,
) ([]evaluationScenario, []evaluationSolver, error) {
	scenarios, solvers, err := qualificationManifestSolvers(plan, manifest)
	if err != nil {
		return nil, nil, err
	}
	if manifest.Campaign != qualificationFixedGate &&
		manifest.Campaign != qualificationIndependent {
		return nil, nil, fmt.Errorf("unknown qualification campaign %q", manifest.Campaign)
	}
	if manifest.Campaign == qualificationFixedGate &&
		manifest.SeedStart != plan.SeedStart {
		return nil, nil, fmt.Errorf("fixed gate seed start = %d, want %d",
			manifest.SeedStart, plan.SeedStart)
	}
	if manifest.Campaign == qualificationIndependent {
		fixedEnd, _ := evaluationSeedEnd(plan.SeedStart, plan.Runs)
		if manifest.SeedStart < fixedEnd && manifest.SeedEnd > plan.SeedStart {
			return nil, nil, fmt.Errorf("independent campaign overlaps fixed gate")
		}
	}
	want, err := buildQualificationManifest(
		plan, scenarios, solvers, manifest.Campaign, manifest.SeedStart)
	if err != nil {
		return nil, nil, err
	}
	if manifest.Schema == qualificationLegacyMetaSchema {
		want.Schema = qualificationLegacyMetaSchema
	}
	if !reflect.DeepEqual(manifest, want) {
		return nil, nil, fmt.Errorf("manifest does not match the current source and plan")
	}
	return scenarios, solvers, nil
}

func evaluationTruthRecordFor(truth evaluationTruth) evaluationTruthRecord {
	targets := make([]evaluationTargetTruthRecord, len(truth.Targets))
	for i, target := range truth.Targets {
		targets[i] = evaluationTargetTruthRecord{
			Index: target.Index, Direction: target.Direction.String(), Scale: target.Scale,
			Scored: target.Scored, Qualifies: target.Qualifies,
		}
	}
	return evaluationTruthRecord{Targets: targets}
}

func classifyEvaluationRecord(record evaluationRunRecord) evaluationClassification {
	switch record.Result.Outcome {
	case OutlierFound.String():
		if len(record.Truth.Targets) == 0 {
			return evaluationFalseFind
		}
		for _, target := range record.Truth.Targets {
			if target.Index != record.Result.Index {
				continue
			}
			if target.Direction != record.Result.Direction {
				return evaluationWrongSign
			}
			if target.Scored && !target.Qualifies {
				return evaluationSubedgeFound
			}
			return evaluationCorrectFound
		}
		return evaluationWrongItem
	case OutlierNegative.String():
		if len(record.Truth.Targets) == 0 {
			return evaluationCorrectNegative
		}
		for _, target := range record.Truth.Targets {
			if target.Scored && target.Qualifies {
				return evaluationFalseNegative
			}
			if target.Scored {
				return evaluationSubedgeNegative
			}
		}
		return evaluationUnscoredNegative
	case OutlierInconclusive.String():
		return evaluationInconclusive
	default:
		return evaluationInvalidResult
	}
}

func validateQualificationRecord(
	manifest qualificationManifest, scenario evaluationScenario,
	solver evaluationSolver, record evaluationRunRecord,
) error {
	if record.Schema != evaluationRunSchema && record.Schema != evaluationLegacyRunSchema {
		return fmt.Errorf("record schema = %q", record.Schema)
	}
	if record.Scenario != scenario.Name || record.Solver != solver.Name ||
		!reflect.DeepEqual(record.SolverParams, solver.Params) {
		return fmt.Errorf("record identity does not match manifest selection")
	}
	if record.Run < 0 || record.Run >= manifest.Runs {
		return fmt.Errorf("run index %d outside [0, %d)", record.Run, manifest.Runs)
	}
	seed := evaluationRunSeed(manifest.SeedStart, record.Run)
	environmentSeed, solverSeed := evaluationSeeds(manifest.SeedStart, record.Run)
	if record.Seed != seed || record.EnvironmentSeed != environmentSeed ||
		record.SolverSeed != solverSeed {
		return fmt.Errorf("record seed derivation mismatch")
	}
	truth := scenario.truth(environmentSeed)
	if !reflect.DeepEqual(record.Truth, evaluationTruthRecordFor(truth)) {
		return fmt.Errorf("record truth does not match its environment seed")
	}
	wantRequest := evaluationRequestRecord{
		Population: scenario.Population, Direction: scenario.RequestDirection.String(),
		FalseFindRisk:     scenario.FalseFindRisk,
		FalseNegativeRisk: scenario.FalseNegativeRisk,
	}
	if !reflect.DeepEqual(record.Request, wantRequest) ||
		!reflect.DeepEqual(record.Profile, evaluationProfile(scenario)) ||
		record.MinimumEdge != scenario.MinimumEdge ||
		record.MaxComparisons != scenario.MaxComparisons ||
		!reflect.DeepEqual(record.Channel, scenario.Channel) ||
		record.PremiseNote != scenario.PremiseNote {
		return fmt.Errorf("record scenario inputs do not match the manifest")
	}
	if record.Result.Comparisons != record.ObservedChannel.Comparisons ||
		record.Result.Participations != record.ObservedChannel.Participations ||
		record.Cost.Comparisons != record.ObservedChannel.Comparisons ||
		record.Cost.Participations != record.ObservedChannel.Participations ||
		record.ObservedChannel.Comparisons > record.MaxComparisons {
		return fmt.Errorf("record accounting is inconsistent")
	}
	if record.Classification != evaluationSolverError &&
		record.Classification != evaluationAccountingError &&
		record.Classification != evaluationInvalidResult &&
		record.Classification != evaluationUnsupported {
		if got := classifyEvaluationRecord(record); got != record.Classification {
			return fmt.Errorf("classification = %q, recomputed %q",
				record.Classification, got)
		}
	}
	return nil
}

func qualificationDecision(
	plan qualificationPlan, results qualificationResults,
) ([]qualificationCellSummary, []qualificationFailureSummary, []string) {
	falseAccept := qualificationFalseAccept / float64(plan.statisticalCells())
	var cells []qualificationCellSummary
	var failures []qualificationFailureSummary
	var violations []string
	for _, planned := range plan.Scenarios {
		for _, solver := range plan.Fleet {
			result := results[qualificationCellKey{planned.Name, solver}]
			if result == nil || result.Records != plan.Runs {
				got := 0
				if result != nil {
					got = result.Records
				}
				violations = append(violations, fmt.Sprintf(
					"scenario=%s solver=%s records=%d want=%d",
					planned.Name, solver, got, plan.Runs))
				continue
			}
			if result.StructuralErrors != 0 {
				failures = append(failures, qualificationFailureSummary{
					Kind:     qualificationDeterministicFailure,
					Scenario: planned.Name, Solver: solver,
					Errors: result.StructuralErrors, CriticalErrors: 0,
					Records: slices.Clone(result.StructuralRuns),
				})
				violations = append(violations, fmt.Sprintf(
					"scenario=%s solver=%s structural-errors=%d first=%s",
					planned.Name, solver, result.StructuralErrors,
					result.FirstStructural))
			}
			for _, check := range planned.Checks {
				errors := result.CheckErrors[check.Kind]
				cell := qualificationCellSummary{
					Scenario: planned.Name, Solver: solver, Check: check.Kind,
					Errors: errors, Runs: result.Records,
				}
				if check.Kind == qualificationExhaustion {
					cell.CriticalErrors = 0
					cell.Passed = errors == 0
				} else {
					scenario := evaluationScenarioByName(planned.Name)
					nominal := qualificationNominalBudget(scenario, check)
					design, err := newCalibrationCellPlan(
						plan.Runs, nominal, check.Margin, falseAccept)
					if err != nil {
						violations = append(violations, err.Error())
						continue
					}
					cell.CriticalErrors = design.CriticalErrors
					cell.NominalBudget = nominal
					cell.RegressionRate = nominal + check.Margin
					cell.FalseAcceptRate = falseAccept
					cell.FalseRejectRate = design.FalseRejectRate
					cell.Passed = errors <= design.CriticalErrors
					if design.FalseRejectRate > qualificationFalseReject {
						cell.Passed = false
						violations = append(violations, fmt.Sprintf(
							"scenario=%s solver=%s check=%s false-reject=%.6f exceeds %.6f",
							cell.Scenario, cell.Solver, cell.Check,
							design.FalseRejectRate, qualificationFalseReject))
					}
				}
				if !cell.Passed {
					kind := qualificationSampledRejection
					if check.Kind == qualificationExhaustion {
						kind = qualificationExactRejection
					}
					failures = append(failures, qualificationFailureSummary{
						Kind: kind, Scenario: cell.Scenario, Solver: cell.Solver,
						Check: cell.Check, Errors: cell.Errors,
						CriticalErrors: cell.CriticalErrors,
						Records:        slices.Clone(result.CheckErrorRuns[check.Kind]),
					})
					violations = append(violations, fmt.Sprintf(
						"scenario=%s solver=%s check=%s errors=%d critical=%d",
						cell.Scenario, cell.Solver, cell.Check,
						cell.Errors, cell.CriticalErrors))
				}
				cells = append(cells, cell)
			}
		}
	}
	return cells, failures, violations
}

func evaluationScenarioByName(name string) evaluationScenario {
	for _, scenario := range evaluationScenarioCatalogue() {
		if scenario.Name == name {
			return scenario
		}
	}
	panic("validated qualification scenario is missing: " + name)
}

func verifyQualificationBundleForPlan(
	root string, plan qualificationPlan,
) (qualificationSummary, error) {
	paths, err := qualificationEvidencePaths(root)
	if err != nil {
		return qualificationSummary{}, err
	}
	allScenarios, allSolvers, err := qualificationInputs(plan, "all")
	if err != nil {
		return qualificationSummary{}, err
	}
	results := newQualificationResults(plan, allScenarios, allSolvers, true)
	scenarioByName := make(map[string]evaluationScenario, len(allScenarios))
	for _, scenario := range allScenarios {
		scenarioByName[scenario.Name] = scenario
	}
	solverByName := make(map[string]evaluationSolver, len(allSolvers))
	for _, solver := range allSolvers {
		solverByName[solver.Name] = solver
	}
	seenRecords := make(map[string]bool,
		len(plan.Fleet)*len(plan.Scenarios)*plan.Runs)
	seenSolvers := make(map[string]bool, len(plan.Fleet))
	var first *qualificationManifest
	totalRecords := 0
	for _, path := range paths {
		evidence, err := readQualificationEvidence(path,
			func(manifest qualificationManifest) error {
				_, _, err := validateQualificationManifest(plan, manifest)
				if err != nil {
					return err
				}
				if first == nil {
					copy := manifest
					first = &copy
				} else if manifest.PlanID != first.PlanID ||
					manifest.Campaign != first.Campaign ||
					manifest.SeedStart != first.SeedStart ||
					manifest.SeedEnd != first.SeedEnd ||
					manifest.SourceSHA256 != first.SourceSHA256 ||
					manifest.Revision != first.Revision || manifest.Dirty != first.Dirty ||
					manifest.GoVersion != first.GoVersion || manifest.GOOS != first.GOOS ||
					manifest.GOARCH != first.GOARCH {
					return fmt.Errorf("evidence manifests describe different campaigns")
				}
				for _, solver := range manifest.Shard {
					if seenSolvers[solver] {
						return fmt.Errorf("solver %q appears in more than one shard", solver)
					}
					seenSolvers[solver] = true
				}
				return nil
			},
			func(manifest qualificationManifest, record evaluationRunRecord) error {
				scenario, scenarioOK := scenarioByName[record.Scenario]
				solver, solverOK := solverByName[record.Solver]
				if !scenarioOK || !solverOK || !slices.Contains(manifest.Shard, record.Solver) {
					return fmt.Errorf("unexpected solver or scenario")
				}
				if err := validateQualificationRecord(
					manifest, scenario, solver, record); err != nil {
					return err
				}
				key := fmt.Sprintf("%s\x00%s\x00%d",
					record.Solver, record.Scenario, record.Seed)
				if seenRecords[key] {
					return fmt.Errorf("duplicate trial %s/%s/%d",
						record.Solver, record.Scenario, record.Seed)
				}
				seenRecords[key] = true
				results.add(record)
				totalRecords++
				return nil
			})
		if err != nil {
			return qualificationSummary{}, fmt.Errorf("verify %s: %w", path, err)
		}
		wantRecords := evidence.Manifest.Runs * len(evidence.Manifest.Scenarios) *
			len(evidence.Manifest.Shard)
		if evidence.Completion.Records != wantRecords {
			return qualificationSummary{}, fmt.Errorf(
				"verify %s: records = %d, want %d", path,
				evidence.Completion.Records, wantRecords)
		}
	}
	if first == nil {
		return qualificationSummary{}, fmt.Errorf("qualification evidence is empty")
	}
	for _, solver := range plan.Fleet {
		if !seenSolvers[solver] {
			return qualificationSummary{}, fmt.Errorf("qualification fleet is missing %q", solver)
		}
	}
	if len(seenSolvers) != len(plan.Fleet) {
		return qualificationSummary{}, fmt.Errorf("qualification evidence has unexpected solvers")
	}
	cells, failures, violations := qualificationDecision(plan, results)
	result := "pass"
	if len(violations) != 0 {
		result = "reject"
	}
	summary := qualificationSummary{
		Schema: qualificationSummarySchema, PlanID: first.PlanID,
		Campaign: first.Campaign, SourceSHA256: first.SourceSHA256,
		Revision: first.Revision, SeedStart: first.SeedStart, SeedEnd: first.SeedEnd,
		Runs: first.Runs, Fleet: slices.Clone(first.Fleet),
		EvidenceFiles: len(paths), Records: totalRecords, Cells: cells,
		Failures:   failures,
		Violations: violations, Result: result,
	}
	if len(violations) != 0 {
		return summary, fmt.Errorf("qualification evidence rejected with %d violations",
			len(violations))
	}
	return summary, nil
}

func writeQualificationSummary(path string, summary qualificationSummary) error {
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeNewQualificationFile(path, data)
}

func writeQualificationDisposition(
	path string, summary qualificationSummary, verifyErr error,
) error {
	var report strings.Builder
	report.WriteString("# Ranking qualification disposition\n\n")
	if summary.Schema == "" {
		report.WriteString("- Evidence validation: failed\n")
		report.WriteString("- Gate result: unavailable\n")
	} else {
		fmt.Fprintf(&report, "- Plan: `%s`\n", summary.PlanID)
		fmt.Fprintf(&report, "- Campaign: `%s`\n", summary.Campaign)
		fmt.Fprintf(&report, "- Source SHA-256: `%s`\n", summary.SourceSHA256)
		fmt.Fprintf(&report, "- Revision: `%s`\n", summary.Revision)
		fmt.Fprintf(&report, "- Seed interval: `[%d, %d)`\n",
			summary.SeedStart, summary.SeedEnd)
		fmt.Fprintf(&report, "- Gate result: `%s`\n", summary.Result)
		fmt.Fprintf(&report, "- Failure groups: %d\n", len(summary.Failures))
	}
	if verifyErr != nil {
		report.WriteString("- Verification completed successfully: no\n")
	} else {
		report.WriteString("- Verification completed successfully: yes\n")
	}
	report.WriteString("\n## Evidence\n\n")
	report.WriteString("- Artefact or local path:\n")
	report.WriteString("- Machine-readable summary:\n")
	if len(summary.Failures) != 0 {
		report.WriteString("\n## Reported failures\n\n")
		for _, failure := range summary.Failures {
			fmt.Fprintf(&report, "- `%s`: scenario `%s`, solver `%s`",
				failure.Kind, failure.Scenario, failure.Solver)
			if failure.Check != "" {
				fmt.Fprintf(&report, ", check `%s`", failure.Check)
			}
			fmt.Fprintf(&report, ", errors %d, critical %d, seeds:",
				failure.Errors, failure.CriticalErrors)
			for _, record := range failure.Records {
				fmt.Fprintf(&report, " `%d`", record.Seed)
			}
			report.WriteString("\n")
		}
	}
	report.WriteString("\n## Disposition\n\n")
	report.WriteString("- Classification: TODO\n")
	report.WriteString("- Exact reproduction: TODO\n")
	report.WriteString("- Scope and affected contracts: TODO\n")
	report.WriteString("- Production, harness, or statistical analysis: TODO\n")
	report.WriteString("- Release impact: TODO\n")
	report.WriteString("- Corrective action: TODO\n")
	report.WriteString("- Reviewer: TODO\n")
	return writeNewQualificationFile(path, []byte(report.String()))
}

func writeNewQualificationFile(path string, data []byte) error {
	// Path is an explicit caller-selected report destination.
	file, err := os.OpenFile( //nolint:gosec // Deliberate output path.
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func printQualificationSummary(summary qualificationSummary) {
	fmt.Printf("qualification-verification=%s plan=%s campaign=%s "+
		"seed-window=[%d,%d) files=%d records=%d cells=%d violations=%d\n",
		summary.Result, summary.PlanID, summary.Campaign, summary.SeedStart,
		summary.SeedEnd, summary.EvidenceFiles, summary.Records,
		len(summary.Cells), len(summary.Violations))
	for _, violation := range summary.Violations {
		fmt.Printf("qualification-violation=%s\n", violation)
	}
	for _, failure := range summary.Failures {
		fmt.Printf("qualification-failure=kind=%s scenario=%s solver=%s check=%s "+
			"errors=%d critical=%d\n", failure.Kind, failure.Scenario,
			failure.Solver, failure.Check, failure.Errors, failure.CriticalErrors)
		for _, record := range failure.Records {
			fmt.Printf("qualification-failure-record=scenario=%s solver=%s "+
				"check=%s run=%d seed=%d classification=%s error=%q\n",
				failure.Scenario, failure.Solver, failure.Check, record.Run,
				record.Seed, record.Classification, record.Error)
		}
	}
	var tightest *qualificationCellSummary
	for i := range summary.Cells {
		cell := &summary.Cells[i]
		if cell.Check == qualificationExhaustion {
			continue
		}
		if tightest == nil ||
			cell.CriticalErrors-cell.Errors < tightest.CriticalErrors-tightest.Errors {
			tightest = cell
		}
	}
	if tightest != nil {
		fmt.Printf("qualification-tightest-cell=scenario=%s solver=%s check=%s "+
			"errors=%d critical=%d\n", tightest.Scenario, tightest.Solver,
			tightest.Check, tightest.Errors, tightest.CriticalErrors)
	}
}

func TestOutlierSolverQualificationVerify(t *testing.T) {
	if *qualificationVerifyPathFlag == "" {
		t.Skip("set -qualification-verify.path to verify qualification evidence")
	}
	plan, err := selectedQualificationPlan()
	if err != nil {
		t.Fatal(err)
	}
	summary, verifyErr := verifyQualificationBundleForPlan(
		*qualificationVerifyPathFlag, plan)
	if summary.Schema == "" && verifyErr != nil {
		fmt.Printf("qualification-verification=invalid-evidence error=%q\n", verifyErr)
	}
	if summary.Schema != "" {
		printQualificationSummary(summary)
		if *qualificationVerifySummaryFlag != "" {
			if err := writeQualificationSummary(
				*qualificationVerifySummaryFlag, summary); err != nil {
				t.Fatalf("write qualification summary: %v", err)
			}
		}
	}
	if *qualificationVerifyDispositionFlag != "" {
		if err := writeQualificationDisposition(
			*qualificationVerifyDispositionFlag, summary, verifyErr); err != nil {
			t.Fatalf("write qualification disposition: %v", err)
		}
	}
	if verifyErr != nil {
		t.Fatal(verifyErr)
	}
}

func TestOutlierSolverQualificationReplay(t *testing.T) {
	if *qualificationReplayPathFlag == "" {
		t.Skip("set -qualification-replay.path to replay qualification evidence")
	}
	if *qualificationReplayScenarioFlag == "" || *qualificationReplaySolverFlag == "" ||
		*qualificationReplaySeedFlag == "" {
		t.Fatal("replay scenario, solver, and seed are required")
	}
	seed, err := strconv.ParseUint(*qualificationReplaySeedFlag, 10, 64)
	if err != nil {
		t.Fatalf("replay seed: %v", err)
	}
	plan, err := selectedQualificationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if err := replayQualificationRecord(*qualificationReplayPathFlag,
		*qualificationReplayScenarioFlag, *qualificationReplaySolverFlag,
		seed, plan); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("qualification-replay=match scenario=%s solver=%s seed=%d\n",
		*qualificationReplayScenarioFlag, *qualificationReplaySolverFlag, seed)
}

func replayQualificationRecord(
	path, scenarioName, solverName string, seed uint64, plan qualificationPlan,
) error {
	var found *evaluationRunRecord
	var foundManifest qualificationManifest
	_, err := readQualificationEvidence(path,
		func(manifest qualificationManifest) error {
			_, _, err := validateQualificationManifest(plan, manifest)
			return err
		},
		func(manifest qualificationManifest, record evaluationRunRecord) error {
			if record.Scenario == scenarioName &&
				record.Solver == solverName && record.Seed == seed {
				if found != nil {
					return fmt.Errorf("replay record appears more than once")
				}
				copy := record
				found = &copy
				foundManifest = manifest
			}
			return nil
		})
	if err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("record scenario=%s solver=%s seed=%d not found",
			scenarioName, solverName, seed)
	}
	scenario := evaluationScenarioByName(found.Scenario)
	_, solvers, err := qualificationManifestSolvers(plan, foundManifest)
	if err != nil {
		return err
	}
	var solver evaluationSolver
	for _, candidate := range solvers {
		if candidate.Name == found.Solver {
			solver = candidate
		}
	}
	got := runEvaluationTrialWithDriver(context.Background(), solver, scenario,
		foundManifest.SeedStart, found.Run,
		qualificationEvaluationDriver(plan, solver))
	want := *found
	want.Schema = evaluationRunSchema
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("replay mismatch: recorded source/environment did not "+
			"reproduce deterministically\nrecorded: %+v\nreplayed: %+v", *found, got)
	}
	return nil
}

func qualificationEvidenceTestPlan() qualificationPlan {
	return qualificationPlan{
		ID: qualificationPlanID, Driver: qualificationDriverID,
		Runs: 50, SeedStart: qualificationSeedStart,
		Fleet: []string{"baseline-reserved"},
		Scenarios: []qualificationScenario{{
			Name: "gaussian/m16/either/strong/one-sided-reversed-control1/w4/p1",
			Checks: []qualificationCheck{{
				Kind: qualificationUtility, Budget: 1e-9, Margin: 0.49,
			}},
		}},
	}
}

func writeQualificationEvidenceForTest(
	t *testing.T, path string, plan qualificationPlan, shard string, seedStart uint64,
	mutate func(int, *evaluationRunRecord),
) {
	t.Helper()
	scenarios, solvers, err := qualificationInputs(plan, shard)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := buildQualificationManifest(
		plan, scenarios, solvers, qualificationIndependent, seedStart)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := newEvidenceWriter(
		path, manifest, len(scenarios)*len(solvers)*plan.Runs)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Abort()
	index := 0
	for _, scenario := range scenarios {
		for run := range plan.Runs {
			for _, solver := range solvers {
				record := runEvaluationTrialWithDriver(
					t.Context(), solver, scenario, seedStart, run,
					evaluationResumableDriver)
				if mutate != nil {
					mutate(index, &record)
				}
				if err := writer.Write(record); err != nil {
					t.Fatal(err)
				}
				index++
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestQualificationEvidenceBundleAndReplay(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	directory := t.TempDir()
	path := filepath.Join(directory, "baseline-reserved.jsonl")
	const seedStart = uint64(10_001)
	writeQualificationEvidenceForTest(t, path, plan, "all", seedStart, nil)
	summary, err := verifyQualificationBundleForPlan(directory, plan)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Result != "pass" || summary.Records != plan.Runs ||
		len(summary.Cells) != 1 || !summary.Cells[0].Passed {
		t.Fatalf("summary = %+v", summary)
	}
	if err := replayQualificationRecord(path, plan.Scenarios[0].Name,
		plan.Fleet[0], seedStart+17, plan); err != nil {
		t.Fatal(err)
	}
	// The test plan has a small positive run count.
	runs := uint64(plan.Runs) //nolint:gosec // Test-controlled positive count.
	if err := replayQualificationRecord(path, plan.Scenarios[0].Name,
		plan.Fleet[0], seedStart+runs, plan); err == nil {
		t.Fatal("replay accepted a seed outside the evidence")
	}
}

func TestEvidenceWriterRemovesIncompleteOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incomplete.jsonl")
	writer, err := newEvidenceWriter(path, qualificationManifest{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err == nil {
		t.Fatal("incomplete writer close succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete output remains: %v", err)
	}
}

func TestQualificationEvidenceRejectsStatisticalFailure(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	directory := t.TempDir()
	path := filepath.Join(directory, "reject.jsonl")
	writeQualificationEvidenceForTest(t, path, plan, "all", 20_001,
		func(_ int, record *evaluationRunRecord) {
			record.Result.Outcome = OutlierInconclusive.String()
			record.Result.Index = -1
			record.Result.Direction = ""
			record.Classification = evaluationInconclusive
		})
	summary, err := verifyQualificationBundleForPlan(directory, plan)
	if err == nil {
		t.Fatal("statistically rejecting evidence passed")
	}
	if summary.Result != "reject" || len(summary.Violations) == 0 ||
		summary.Cells[0].Errors != plan.Runs || len(summary.Failures) != 1 ||
		summary.Failures[0].Kind != qualificationSampledRejection ||
		len(summary.Failures[0].Records) != plan.Runs {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestQualificationEvidenceDoesNotPromotePermittedError(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	directory := t.TempDir()
	path := filepath.Join(directory, "permitted.jsonl")
	writeQualificationEvidenceForTest(t, path, plan, "all", 25_001,
		func(index int, record *evaluationRunRecord) {
			if index == 0 {
				record.Result.Outcome = OutlierInconclusive.String()
				record.Result.Index = -1
				record.Result.Direction = ""
				record.Classification = evaluationInconclusive
			}
		})
	summary, err := verifyQualificationBundleForPlan(directory, plan)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cells[0].Errors == 0 ||
		summary.Cells[0].Errors > summary.Cells[0].CriticalErrors ||
		len(summary.Failures) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestQualificationEvidenceClassifiesDeterministicFailure(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	path := filepath.Join(t.TempDir(), "solver-error.jsonl")
	writeQualificationEvidenceForTest(t, path, plan, "all", 26_001,
		func(index int, record *evaluationRunRecord) {
			if index == 0 {
				record.Classification = evaluationSolverError
				record.Error = "synthetic solver failure"
			}
		})
	summary, err := verifyQualificationBundleForPlan(path, plan)
	if err == nil {
		t.Fatal("deterministic failure passed")
	}
	if len(summary.Failures) == 0 ||
		summary.Failures[0].Kind != qualificationDeterministicFailure ||
		len(summary.Failures[0].Records) != 1 ||
		summary.Failures[0].Records[0].Seed != 26_001 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestQualificationEvidenceClassifiesExactFailure(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	plan.Scenarios[0].Checks = append(
		plan.Scenarios[0].Checks, qualificationCheck{Kind: qualificationExhaustion})
	path := filepath.Join(t.TempDir(), "exact.jsonl")
	writeQualificationEvidenceForTest(t, path, plan, "all", 27_001, nil)
	summary, err := verifyQualificationBundleForPlan(path, plan)
	if err == nil {
		t.Fatal("exact failure passed")
	}
	if len(summary.Failures) != 1 ||
		summary.Failures[0].Kind != qualificationExactRejection ||
		len(summary.Failures[0].Records) == 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestQualificationDisposition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disposition.md")
	summary := qualificationSummary{
		Schema: qualificationSummarySchema, PlanID: qualificationPlanID,
		Campaign: qualificationIndependent, SourceSHA256: "source",
		SeedStart: 10_001, SeedEnd: 13_001, Result: "reject",
		Failures: []qualificationFailureSummary{{
			Kind: qualificationSampledRejection, Scenario: "scenario",
			Solver: "solver", Check: qualificationFalseFind,
			Errors: 3, CriticalErrors: 2,
			Records: []qualificationFailureRecord{{
				Run: 7, Seed: 10_008, Classification: evaluationFalseFind,
			}},
		}},
	}
	if err := writeQualificationDisposition(path, summary, errors.New("reject")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // Test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Gate result: `reject`", "`sampled-rejection`", "`10008`",
		"Classification: TODO", "Reviewer: TODO",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("disposition does not contain %q", want)
		}
	}
	if err := writeQualificationDisposition(path, summary, nil); err == nil {
		t.Fatal("disposition overwrote existing file")
	}
}

func TestQualificationEvidenceRejectsIncompleteAndDuplicateData(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	t.Run("truncated", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "truncated.jsonl")
		writeQualificationEvidenceForTest(t, path, plan, "all", 30_001, nil)
		data, err := os.ReadFile(path) //nolint:gosec // Test-owned path.
		if err != nil {
			t.Fatal(err)
		}
		last := bytes.LastIndex(data[:len(data)-1], []byte{'\n'})
		// Path belongs to this test's temporary directory.
		if err := os.WriteFile( //nolint:gosec // Test-owned temporary path.
			path, data[:last+1], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyQualificationBundleForPlan(path, plan); err == nil ||
			!strings.Contains(err.Error(), "missing completion") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "digest.jsonl")
		writeQualificationEvidenceForTest(t, path, plan, "all", 35_001, nil)
		data, err := os.ReadFile(path) //nolint:gosec // Test-owned path.
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(data, []byte{'\n'})
		var completion evidenceCompletion
		if err := json.Unmarshal(lines[len(lines)-2], &completion); err != nil {
			t.Fatal(err)
		}
		completion.RecordsSHA256 = strings.Repeat("0", sha256.Size*2)
		line, err := canonicalJSONLine(completion)
		if err != nil {
			t.Fatal(err)
		}
		lines[len(lines)-2] = bytes.TrimSuffix(line, []byte{'\n'})
		// Path belongs to this test's temporary directory.
		if err := os.WriteFile( //nolint:gosec // Test-owned temporary path.
			path, bytes.Join(lines, []byte{'\n'}), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyQualificationBundleForPlan(path, plan); err == nil ||
			!strings.Contains(err.Error(), "record digest mismatch") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("duplicate record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "duplicate.jsonl")
		var first evaluationRunRecord
		writeQualificationEvidenceForTest(t, path, plan, "all", 40_001,
			func(index int, record *evaluationRunRecord) {
				switch index {
				case 0:
					first = *record
				case 1:
					*record = first
				}
			})
		if _, err := verifyQualificationBundleForPlan(path, plan); err == nil ||
			!strings.Contains(err.Error(), "duplicate trial") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("duplicate shard", func(t *testing.T) {
		directory := t.TempDir()
		first := filepath.Join(directory, "first.jsonl")
		second := filepath.Join(directory, "second.jsonl")
		writeQualificationEvidenceForTest(t, first, plan, "all", 50_001, nil)
		data, err := os.ReadFile(first) //nolint:gosec // Test-owned path.
		if err != nil {
			t.Fatal(err)
		}
		// Second belongs to this test's temporary directory.
		if err := os.WriteFile( //nolint:gosec // Test-owned temporary path.
			second, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyQualificationBundleForPlan(directory, plan); err == nil ||
			!strings.Contains(err.Error(), "more than one shard") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unexpected record", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unexpected.jsonl")
		writeQualificationEvidenceForTest(t, path, plan, "all", 55_001,
			func(index int, record *evaluationRunRecord) {
				if index == 0 {
					record.Solver = "peer-first"
				}
			})
		if _, err := verifyQualificationBundleForPlan(path, plan); err == nil ||
			!strings.Contains(err.Error(), "unexpected solver or scenario") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing fleet shard", func(t *testing.T) {
		twoSolverPlan := plan
		twoSolverPlan.Fleet = []string{"baseline-reserved", "peer-first"}
		path := filepath.Join(t.TempDir(), "baseline-reserved.jsonl")
		writeQualificationEvidenceForTest(
			t, path, twoSolverPlan, "baseline-reserved", 56_001, nil)
		if _, err := verifyQualificationBundleForPlan(path, twoSolverPlan); err == nil ||
			!strings.Contains(err.Error(), "fleet is missing") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("mixed campaign windows", func(t *testing.T) {
		twoSolverPlan := plan
		twoSolverPlan.Fleet = []string{"baseline-reserved", "peer-first"}
		directory := t.TempDir()
		writeQualificationEvidenceForTest(t,
			filepath.Join(directory, "baseline-reserved.jsonl"), twoSolverPlan,
			"baseline-reserved", 57_001, nil)
		writeQualificationEvidenceForTest(t,
			filepath.Join(directory, "peer-first.jsonl"), twoSolverPlan,
			"peer-first", 60_001, nil)
		if _, err := verifyQualificationBundleForPlan(directory, twoSolverPlan); err == nil ||
			!strings.Contains(err.Error(), "different campaigns") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestQualificationManifestVerificationRejectsMismatch(t *testing.T) {
	plan := qualificationEvidenceTestPlan()
	scenarios, solvers, err := qualificationInputs(plan, "all")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := buildQualificationManifest(
		plan, scenarios, solvers, qualificationIndependent, 60_001)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*qualificationManifest)
	}{
		{"schema", func(m *qualificationManifest) { m.Schema = "unknown" }},
		{"plan", func(m *qualificationManifest) { m.PlanID = "unknown" }},
		{"source", func(m *qualificationManifest) { m.SourceSHA256 = "unknown" }},
		{"environment", func(m *qualificationManifest) { m.GOOS = "unknown" }},
		{"window", func(m *qualificationManifest) { m.SeedEnd++ }},
		{"overlap", func(m *qualificationManifest) {
			m.SeedStart = plan.SeedStart
			// The test plan has a small positive run count.
			runs := uint64(plan.Runs) //nolint:gosec // Test-controlled count.
			m.SeedEnd = plan.SeedStart + runs
		}},
		{"unknown solver", func(m *qualificationManifest) { m.Shard[0] = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := manifest
			changed.Shard = slices.Clone(manifest.Shard)
			test.mutate(&changed)
			if _, _, err := validateQualificationManifest(plan, changed); err == nil {
				t.Fatal("mismatched manifest accepted")
			}
		})
	}
}
