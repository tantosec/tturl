# tturl structured-output schemas

These JSON Schema Draft 2020-12 documents describe the versioned structured
output produced by the request commands.

| Schema identifier | Document | Framing |
| --- | --- | --- |
| `tturl.race/v1` | [race v1](tturl-race-v1.schema.json) | JSON Lines records |
| `tturl.measure/v1` | [measure v1](tturl-measure-v1.schema.json) | JSON Lines records |
| `tturl.analyse/v1` | [analyse v1](tturl-analyse-v1.schema.json) | JSON Lines records |
| `tturl.detect/v1` | [detect v1](tturl-detect-v1.schema.json) | JSON Lines records |
| `tturl.time/v1` | [time v1](tturl-time-v1.schema.json) | JSON Lines records |

The command schemas reference
[`tturl-common-v1.schema.json`](tturl-common-v1.schema.json). The
[whole-stream contract](../structured-output-contract.md) defines ordering,
identity, reference integrity and arithmetic. [Checked examples](examples/)
illustrate completion and scientific states.

## Retrieve schemas offline

Use the binary that produced the report to export its exact shipped contracts:

```sh
tturl schema race --output race.schema.json
tturl schema common -o common.schema.json
```

Replace `race` with `measure`, `analyse`, `detect` or `time` for the selected
command.
The consumer needs that command document and common v1; those are the only
project-schema dependencies of the current family. Common is a definitions
resource, not a report schema on its own. Export preserves the documents' bytes
and identities. It does not validate reports or select historical contracts.

Register both documents under their original `$id` values with a Draft 2020-12
resolver. Relative `$ref` values resolve against those identifiers; saving the
files beside each other alone does not guarantee offline resolution. The
report's logical `schema` value differs from a document's `$id` URI, and export
neither requires nor promises network retrieval of that URI. See the
[normative stream contract](../structured-output-contract.md) and
`tturl help schema` for resolver guidance and whole-stream requirements.

The JSON Lines schemas validate each line as one allowed record. They do not
express ordering or cardinality across lines. A complete stream starts with one
`run` record, continues with its command-specific records, and ends with the
terminal `result` described by the command help. An interrupted
run still has a terminal record when the report destination remains writable.
Only a report-channel failure or a consumer-controlled closed pipe can leave a
valid prefix without one.

The `schema` member is the stable contract identifier. It is deliberately not a
URL. Consumers should select the exactly matching document from this index and
reject or preserve, without reinterpretation, an identifier they do not
understand.

Schema versions are independent of the tturl binary version. A binary release
can retain an output schema when its contract is unchanged or introduce a new
schema identifier when fields or their meaning change. Once published, schema
files and the shared definitions they reference remain immutable so retained
reports can still be validated.
