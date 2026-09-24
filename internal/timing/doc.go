// Package timing owns internal request-duration transport and trial scheduling.
// It validates typed plans and keeps controlled writes, monotonic observations,
// independent phase budgets and connection lifetimes separate from command
// presentation.
package timing
