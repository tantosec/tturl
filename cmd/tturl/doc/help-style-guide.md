# tturl help library style guide

This guide governs root help, command usage screens, the help index, and
long-form help. Together they must let a reader operate and interpret tturl
with only the executable and a shell.

Readers include penetration testing practitioners, statisticians and academic
researchers, and AI systems. Give them enough context to choose a command,
construct and bound the experiment, inspect the evidence, interpret it
correctly, and find the next level of detail.

## Voice

Use a calm, direct, technically serious voice. Assume a capable reader who may
be new to timeless timing. Prefer plain words, short declarative sentences,
concrete subjects, and exact quantities. Put a qualification beside the claim
it limits.

The getting-started page is the deliberate exception to the otherwise plain
tone. Preserve its established warmth and humour while keeping instructions,
safety guidance, and technical claims literal.

Spell tturl in lowercase, including at the start of a sentence. Use **command**
for a capability and **run** for one invocation. Identify a subcommand by its
full command form, such as tturl race. When only its name is appropriate, call
it the 'race' command.

Single-quote option tokens in prose, including attached values:
'--flag=value'. A separate value remains separate: '--trials' N. Use uppercase
metavariables such as FILE.

## Information architecture

Give each surface one job:

- Root help maps tturl's capabilities and states its model briefly.
- A command usage screen is the complete grammar and option reference.
- The help index maps concepts and tasks in a reader's vocabulary.
- A long-form page teaches a decision, mechanism, workflow, or interpretation.

Give each explanation one authoritative home. Elsewhere, include only the
context needed for the page to stand alone and a literal tturl help TOPIC route
when more detail would help. Place a caution where the reader makes the choice
it governs.

Organise a page around the reader's workflow. Lead with purpose and the most
important interpretive boundary; then cover choices, operation, evidence, and
next steps in that order. Put normal use before exceptions. Use headings that
describe reader tasks, and use lists or tables for genuinely repeated
mappings or accounting.

## Precision

Check behavioural claims against their owning implementation, statistical
contract, reporter, or schema. State every default and limit with its
conditions, unit, and scope. Distinguish requested, planned, attempted,
completed, retained, and excluded work whenever the difference affects use or
interpretation.

Keep description, inference, and vulnerability conclusions separate. For an
inferential result, identify the experimental conditions, assignment or
selection mechanism, premise or null, error guarantee, stopping rule, and the
limits of positive and negative outcomes. State dependencies and assumptions
even when a familiar assumption is unnecessary.

Use stable project terminology and define specialised terms at their first
use on the page that owns them. Use earlier and later for arrival direction.
Describe observable behaviour rather than inferred causality, elapsed-time
magnitude, exploitability, or practical importance.

## Examples and automation

Prefer small, bounded examples against the local demo server. Introduce the
lesson before the command and say what evidence to inspect afterwards. Label
illustrative output and shell-specific syntax. Make workload multiplication
visible when it materially changes target exposure.

Structured-output guidance must identify framing, schema dispatch, record
order, joins, completion state, unavailable values, byte representation,
output channels, partial results, and sensitive data. Examples should process
streams incrementally and preserve stderr separately from machine-readable
stdout.

## Review

Render every changed surface and read it in context with adjacent pages. Run
each example or verify it against a purpose-built local fixture. Then review
the prose separately for:

1. correctness of every fact, number, unit, condition, and claim;
2. usefulness and safety for an operator;
3. statistical interpretation and claim boundaries;
4. explicit framing and navigation for a machine reader;
5. tone, legibility, brevity, redundancy, and fact ownership; and
6. consistency between source, generated usage, reports, and schemas.

Test durable structure and behaviour. Review explanatory prose directly rather
than freezing its sentences, examples, or paragraph order in tests.

Run `go test ./cmd/tturl` after help or output changes. From the repository
root, use `scripts/tturl-tour/generate.sh` for a cross-command visual review.
Update report fixtures deliberately with
`go test ./cmd/tturl -run TestTextReportGoldens -update-report-goldens`.
