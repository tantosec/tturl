package ranking

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
)

type evidenceCompletion struct {
	Schema         string `json:"schema"`
	ManifestSHA256 string `json:"manifest_sha256"`
	RecordsSHA256  string `json:"records_sha256"`
	Records        int    `json:"records"`
}

type evidenceWriter struct {
	path         string
	file         *os.File
	buffer       *bufio.Writer
	recordHash   hash.Hash
	manifestHash string
	expected     int
	records      int
	complete     bool
}

func newEvidenceWriter(path string, manifest any, expected int) (*evidenceWriter, error) {
	if expected < 0 {
		return nil, fmt.Errorf("negative expected evidence record count")
	}
	// Path is an explicit caller-selected evidence destination.
	file, err := os.OpenFile( //nolint:gosec // Deliberate output path.
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	writer := &evidenceWriter{
		path: path, file: file, buffer: bufio.NewWriter(file),
		recordHash: sha256.New(), expected: expected,
	}
	line, err := canonicalJSONLine(manifest)
	if err != nil {
		writer.Abort()
		return nil, fmt.Errorf("encode evidence manifest: %w", err)
	}
	digest := sha256.Sum256(line)
	writer.manifestHash = hex.EncodeToString(digest[:])
	if _, err := writer.buffer.Write(line); err != nil {
		writer.Abort()
		return nil, fmt.Errorf("write evidence manifest: %w", err)
	}
	return writer, nil
}

func canonicalJSONLine(value any) ([]byte, error) {
	line, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

func (w *evidenceWriter) Write(record evaluationRunRecord) error {
	if w.complete {
		return fmt.Errorf("evidence writer is complete")
	}
	if w.records >= w.expected {
		return fmt.Errorf("evidence record count exceeds expected %d", w.expected)
	}
	line, err := canonicalJSONLine(record)
	if err != nil {
		return fmt.Errorf("encode evidence record: %w", err)
	}
	if _, err := w.buffer.Write(line); err != nil {
		return fmt.Errorf("write evidence record: %w", err)
	}
	_, _ = w.recordHash.Write(line)
	w.records++
	return nil
}

func (w *evidenceWriter) Close() error {
	if w.complete {
		return fmt.Errorf("evidence writer is already complete")
	}
	if w.records != w.expected {
		w.Abort()
		return fmt.Errorf("evidence records = %d, want %d", w.records, w.expected)
	}
	completion := evidenceCompletion{
		Schema: evidenceCompletionSchema, ManifestSHA256: w.manifestHash,
		RecordsSHA256: hex.EncodeToString(w.recordHash.Sum(nil)), Records: w.records,
	}
	line, err := canonicalJSONLine(completion)
	if err == nil {
		_, err = w.buffer.Write(line)
	}
	if err == nil {
		err = w.buffer.Flush()
	}
	if err == nil {
		err = w.file.Sync()
	}
	closeErr := w.file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(w.path)
		return fmt.Errorf("finalise evidence: %w", err)
	}
	w.complete = true
	return nil
}

func (w *evidenceWriter) Abort() {
	if w == nil || w.complete {
		return
	}
	_ = w.file.Close()
	_ = os.Remove(w.path)
}

func writeEvaluationJSONL(
	path string, manifest any, records []evaluationRunRecord,
) error {
	writer, err := newEvidenceWriter(path, manifest, len(records))
	if err != nil {
		return err
	}
	defer writer.Abort()
	for _, record := range records {
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	return writer.Close()
}
