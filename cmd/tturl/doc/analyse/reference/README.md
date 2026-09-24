# `tturl analyse` reference

These documents define the supported statistical and structured-output
interfaces and the command's resource boundaries.

## Contracts

- [Statistical method](analyse-method-contract.md)
- [Structured output and data handling](analyse-structured-output.md)
- [JSON Schema v1](../../schemas/tturl-analyse-v1.schema.json)
- [Parallelism resource policy](../../parallelism-resource-policy.md)

These documents are for users integrating the command and maintainers changing
it. Command help, the maintained references and schema, and executable tests
remain authoritative when prose and behaviour disagree.

## Engineering validation

The ordinary test suite checks controlled null and signal workloads,
offline/live result identity, concurrent acquisition, response review,
failure handling, schema conformance, and report modes. Run this maintained
implementation evidence with:

```console
go test ./cmd/tturl
```

These checks validate command mechanics. They do not establish field
sensitivity, calibrate the statistical method, guarantee safe target load, or
guarantee that a selected parallel load preserves a timing signal on an
arbitrary target.
