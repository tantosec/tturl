package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// readStructuredJSONRecord returns only complete newline-terminated records.
// Consumers inspecting a failed destination may discard an incomplete final
// fragment; replay requires the complete stream instead.
func readStructuredJSONRecord(reader *bufio.Reader) (json.RawMessage, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) != 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return json.RawMessage(line), nil
}

// structuredHeader is a decoded framing projection.
type structuredHeader struct {
	Kind         string `json:"kind"`
	RequestCount *int   `json:"request_count"`
	RequestID    *int   `json:"request_id"`
}

// structuredValidator shares the writer's sequencing checks. Command checks
// own roles, references, dimensions and arithmetic, after header validation
// and before sequencing state advances.
type structuredValidator struct {
	sequence     structuredSequence
	terminalKind string
	evidenceKind func(string) bool
	check        func(structuredHeader) error
}

func (v *structuredValidator) Accept(header structuredHeader) error {
	var err error
	switch header.Kind {
	case "run":
		if header.RequestCount == nil {
			return fmt.Errorf("structured run is missing request count")
		}
		err = v.sequence.checkStart(*header.RequestCount)
	case "request":
		err = v.sequence.checkRequest()
		if err == nil && (header.RequestID == nil || *header.RequestID != v.sequence.next) {
			return fmt.Errorf("structured request ID must be %d", v.sequence.next)
		}
	default:
		if header.Kind != v.terminalKind &&
			(v.evidenceKind == nil || !v.evidenceKind(header.Kind)) {
			return fmt.Errorf("structured stream has an unknown record kind")
		}
		err = v.sequence.checkEvidence()
	}
	if err != nil {
		return err
	}
	if v.check != nil {
		if err := v.check(header); err != nil {
			return err
		}
	}
	switch header.Kind {
	case "run":
		v.sequence.started = true
		v.sequence.count = *header.RequestCount
	case "request":
		v.sequence.next++
	default:
		if header.Kind == v.terminalKind {
			v.sequence.finished = true
		}
	}
	return nil
}

func (v *structuredValidator) Finish() error {
	if !v.sequence.finished {
		return fmt.Errorf("structured stream is missing its terminal record")
	}
	return nil
}

// structuredCatalogue checks command role order and global references. The
// framing validator calls Accept before advancing its sequence state.
type structuredCatalogue struct {
	command string
	roles   []string
	group   int
}

func (c *structuredCatalogue) Accept(header structuredHeader, role string) error {
	if header.Kind != "request" {
		return nil
	}
	allowed := []string{"measured", "warmup"}
	if c.command == "detect" {
		allowed = []string{"candidate", "baseline", "warmup"}
	}
	group := -1
	for i, candidate := range allowed {
		if role == candidate {
			group = i
			break
		}
	}
	if group < c.group {
		return fmt.Errorf("structured request role is invalid or out of order")
	}
	c.group = group
	c.roles = append(c.roles, role)
	return nil
}

func (c *structuredCatalogue) CheckID(id int, allowed ...string) error {
	if id < 0 || id >= len(c.roles) {
		return fmt.Errorf("structured request reference is outside the catalogue")
	}
	if slices.Contains(allowed, c.roles[id]) {
		return nil
	}
	return fmt.Errorf("structured request reference has an invalid role")
}
