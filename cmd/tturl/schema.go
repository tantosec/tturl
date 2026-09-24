package main

// schemaDescriptor declares a current wire contract and its canonical resource.
// Common has no logical report identifier.
type schemaDescriptor struct {
	logical  string
	filename string
	document []byte
}
