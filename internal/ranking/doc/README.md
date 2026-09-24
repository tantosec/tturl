# Ranking test evidence

The ranking test apparatus has three layers with different decision authority:

- [Outlier solver evaluation](outliersolver-evaluation.md) describes the shared
  simulator, solver and scenario catalogues, exploratory panels, commands,
  fairness model, and evaluation evidence.
- [Outlier solver qualification](outliersolver-qualification.md) specifies the
  versioned release gate, admitted fleet, statistical design, driver, shards,
  evidence manifest, and admission process.
- [Ranking test evidence policy](ranking-test-evidence-policy.md) states which
  evidence is required during development, CI, releases, investigations, and
  material campaigns.

Use ordinary Go tests for deterministic contracts, evaluation panels to compare
or investigate solvers, and qualification to make an enforcing release
decision. A passing exploratory evaluation is never a substitute for
qualification.
