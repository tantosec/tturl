package main

import _ "embed"

//go:embed doc/schemas/tturl-time-v1.schema.json
var timeSchemaDocument []byte

var timeSchema = schemaDescriptor{
	logical:  "tturl.time/v1",
	filename: "tturl-time-v1.schema.json",
	document: timeSchemaDocument,
}
