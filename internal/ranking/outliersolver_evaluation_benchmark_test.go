package ranking

import "testing"

// BenchmarkOutlierSolverEvaluation reports comparative results through Go's benchmark
// UI. Use an explicit iteration count so every solver sees the same seeds:
//
//	go test ./internal/ranking -run '^$' -bench '^BenchmarkOutlierSolverEvaluation$' \
//		-benchtime=20x -count=1
//
// Custom metrics describe decisions and abstract work; ns/op measures only
// simulator execution time.
func BenchmarkOutlierSolverEvaluation(b *testing.B) {
	scenarios, solvers, err := selectEvaluationInputs(
		*evaluationPanelFlag, *evaluationScenarioFlag, *evaluationSolverFlag,
	)
	if err != nil {
		b.Fatal(err)
	}
	for _, scenario := range scenarios {
		b.Run(scenario.Name, func(b *testing.B) {
			for _, solver := range solvers {
				b.Run(solver.Name, func(b *testing.B) {
					records := runEvaluation(
						b.Context(), []evaluationScenario{scenario},
						[]evaluationSolver{solver}, *evaluationSeedFlag, b.N,
					)
					summary := summariseEvaluation(records)
					denominator := float64(len(records))
					for _, classification := range []evaluationClassification{
						evaluationCorrectFound, evaluationSubedgeFound,
						evaluationCorrectNegative,
						evaluationFalseFind, evaluationWrongItem, evaluationWrongSign,
						evaluationFalseNegative, evaluationSubedgeNegative,
						evaluationUnscoredNegative, evaluationInconclusive,
						evaluationSolverError,
						evaluationAccountingError, evaluationInvalidResult,
						evaluationUnsupported,
					} {
						b.ReportMetric(
							float64(summary.Classifications[classification])/denominator,
							string(classification)+"/op",
						)
					}
					b.ReportMetric(evaluationMean(summary.Comparisons), "comparisons/op")
					b.ReportMetric(
						float64(evaluationQuantile(summary.Comparisons, 0.50)),
						"p50-comparisons/op",
					)
					b.ReportMetric(
						float64(evaluationQuantile(summary.Comparisons, 0.90)),
						"p90-comparisons/op",
					)
					b.ReportMetric(
						float64(evaluationQuantile(summary.Comparisons, 0.99)),
						"p99-comparisons/op",
					)
					b.ReportMetric(
						evaluationMean(summary.Restricted),
						"restricted-comparisons/op",
					)
					b.ReportMetric(
						float64(evaluationQuantile(summary.Restricted, 0.90)),
						"p90-restricted-comparisons/op",
					)
					b.ReportMetric(
						evaluationMean(summary.Participations),
						"participations/op",
					)
					b.ReportMetric(0, "ns/op")
				})
			}
		})
	}
}
