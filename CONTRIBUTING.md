# Contributing

Thanks for contributing to `tturl`.

For substantial work or changes to a public API, please open an issue first
so the approach can be discussed. Small fixes can go directly to a pull
request. Report vulnerabilities privately as described in
[`SECURITY.md`](SECURITY.md), not in a public issue.

Keep changes focused, and include tests and documentation when behaviour
changes. The supported Go library packages are `stats` (domain-free statistical
primitives) and `tth2` (HTTP/2 batch release and arrival-order observations).
Commands and internal packages are consumers, outside that library surface.
Review public changes for both declaration shape and behavioural contracts,
including ownership, copying, concurrency and lifecycle where applicable.
Prefer external-package examples and tests that demonstrate useful composition.
Patch releases preserve source compatibility. A necessary breaking change in a
later pre-1.0 minor release needs an explicit rationale and practical migration
guidance.
Expose the smallest useful public primitive; prefer capabilities that require
access to package internals over features consumers can compose.

The arrival-order commands release a **batch** of **requests** through a shared
client-side gate and use only response **arrival order**. A request's outbound
place is its **position**; one race is a **trial**; responses are **ranked** by
arrival. `tturl time` separately measures from initial request release to
complete, valid final response headers.

Do not leak an application into a generalised package. Identifiers, comments,
documentation, examples, errors, output, and fixtures all count.

Shippable code is pure Go and must build on Linux, macOS, and Windows. The
documented Windows limitations of `tturl time` and `tturl demo-server` do not
relax that build requirement. Use `path/filepath` and Go temporary-directory
APIs rather than platform-specific path assumptions.

Terminal output, help, and errors are ASCII except for the bibliographic
spelling Christina Pöpper.

## Language and spelling

Use Australian English for project-authored code identifiers, comments,
documentation, and user-facing text. For ambiguous spellings, compounds,
hyphenation, and inflections, follow the Australian Government Style Manual
for general guidance and the sixth edition of the *Australian Concise Oxford
Dictionary* as the lexical authority. Use the dictionary's first-listed
headword. For example, use `normalise` for a project-owned operation.

Preserve names owned by external APIs, protocols, standards, dependencies,
programming-language syntax, publications, and people. The HTTP
`Authorization` field, Go's `context.Canceled`, and
`analysis.Analyzer` therefore keep their canonical spellings.

Use **timeless timing attack** in sentence case. Preserve title case only for
the paper title, **Timeless Timing Attacks: Exploiting Concurrency to Leak
Secrets over Remote Connections**.

Describe the current design, not its history. Remove obsolete contrasts and
editing scars. Wrap Go comments at 80 columns, including indentation and `//`.

Run `make spellcheck` to check ordinary spelling and the repository's deliberate
house preferences. Add legitimate recurring technical or project terminology
to the project dictionary narrowly; keep that flat list case-insensitively
sorted with one canonical entry per term. Put synthetic test tokens and exact
externally owned spellings in scoped configuration overrides instead. Do not
use the project dictionary to admit an ordinary US variant or conceal a
spelling error. Run `make spellcheck-audit` after changing either spelling
configuration or a project dictionary.

Run the ordinary contributor gate before opening a pull request:

```console
make check
```

Also run the relevant focused gate when applicable:

- `make fmt` for mechanical formatting;
- `make race` for concurrency changes;
- `make ranking-qualify` for ranking evidence or behaviour, following the
  [ranking test evidence policy](internal/ranking/doc/ranking-test-evidence-policy.md);
  and
- `make nix-deps` after changing Go dependencies, committing the resulting
  `govendor.toml` with `go.mod` and `go.sum`.

Use narrow, justified suppressions: `//nolint:<linter> // <reason>`. Never
exclude paths from gosec; fix a finding or suppress a proven false positive at
its site.

In the pull request, explain what changed, why it changed, and how it was
validated.
