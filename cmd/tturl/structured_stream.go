package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// structuredSequence owns framing, independently of command record shapes.
type structuredSequence struct {
	started  bool
	finished bool
	count    int
	next     int
}

func (s *structuredSequence) checkStart(count int) error {
	if s.finished {
		return fmt.Errorf("structured stream has already finished")
	}
	if s.started {
		return fmt.Errorf("structured stream has already started")
	}
	if count < 0 {
		return fmt.Errorf("structured stream request count must be non-negative")
	}
	return nil
}

func (s *structuredSequence) checkRequest() error {
	if !s.started {
		return fmt.Errorf("structured stream has not started")
	}
	if s.finished {
		return fmt.Errorf("structured stream has already finished")
	}
	if s.next >= s.count {
		return fmt.Errorf("structured stream has excess request records")
	}
	return nil
}

func (s *structuredSequence) checkEvidence() error {
	if !s.started {
		return fmt.Errorf("structured stream has not started")
	}
	if s.finished {
		return fmt.Errorf("structured stream has already finished")
	}
	if s.next != s.count {
		return fmt.Errorf("structured stream request catalogue is incomplete")
	}
	return nil
}

// structuredStream serialises record writes and advances only after successful
// encoding. A failed encode permanently poisons the stream; the destination
// may contain a truncated final record.
type structuredStream struct {
	mu       sync.Mutex
	encoder  *json.Encoder
	sequence structuredSequence
	poison   error
}

func newStructuredStream(w io.Writer) *structuredStream {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return &structuredStream{encoder: encoder}
}

func (s *structuredStream) Start(record any, requestCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return s.poison
	}
	if err := s.sequence.checkStart(requestCount); err != nil {
		return err
	}
	if err := s.encode(record); err != nil {
		return err
	}
	s.sequence.started = true
	s.sequence.count = requestCount
	return nil
}

func (s *structuredStream) Request(record any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return s.poison
	}
	if err := s.sequence.checkRequest(); err != nil {
		return err
	}
	if err := s.encode(record); err != nil {
		return err
	}
	s.sequence.next++
	return nil
}

func (s *structuredStream) Evidence(record any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return s.poison
	}
	if err := s.sequence.checkEvidence(); err != nil {
		return err
	}
	return s.encode(record)
}

func (s *structuredStream) Finish(record any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return s.poison
	}
	if err := s.sequence.checkEvidence(); err != nil {
		return err
	}
	if err := s.encode(record); err != nil {
		return err
	}
	s.sequence.finished = true
	return nil
}

// encode is called only while mu is held and sequence has been checked.
func (s *structuredStream) encode(record any) error {
	if err := s.encoder.Encode(record); err != nil {
		s.poison = err
		return err
	}
	return nil
}
