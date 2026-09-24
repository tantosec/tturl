// Package stats provides domain-free statistical primitives. It includes
// distributions and random sampling, order statistics, online estimators,
// sequential inference, and rank-based models.
//
// Constructors, generators and mutators panic on invalid configuration.
// Evaluators and estimators return NaN for invalid inputs or when a valid call
// has no defined result. Individual functions document their preconditions
// and other domain behaviour.
//
// Gamma distribution functions use shape alpha and rate beta; GammaRV samples
// at unit rate. Beta shapes are alpha and beta. Interval functions use either
// a confidence level or a standard-normal critical value, as named in their
// parameters; sequential constructions use significance level alpha.
//
// Slice arguments are neither modified nor retained unless documented
// otherwise. Sorted primitives trust ascending order without checking it;
// order-statistic samples must be finite. Finite inputs can still overflow or
// underflow intermediate float64 arithmetic. Distribution approximations do
// not promise uniform accuracy at numerical extremes.
//
// Observation counters panic before int64 overflow. Scalar estimators can be
// reused by assigning a fresh zero value when valid, or constructing a fresh
// value otherwise. Sequential stopping guarantees require stopping at the
// first boundary crossing. Independent evidence increments require the stated
// model to hold conditionally on preceding observations.
//
// Mutable values are not safe for concurrent use unless a type documents an
// exception. Their type documentation states whether copying preserves an
// independent value or aliases mutable storage.
package stats
