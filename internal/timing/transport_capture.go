package timing

import (
	"crypto/sha256"
	"hash"
)

// bodyCollector hashes accepted response bytes while retaining only the chosen
// capture prefix. A negative capture limit selects an unlimited prefix.
// Its owner calls accept only after recording reader-side milestones.
type bodyCollector struct {
	digest       hash.Hash
	accepted     int64
	capture      bool
	captureLimit int64
	body         []byte
}

func newBodyCollector(capture bool, limit int64) *bodyCollector {
	return &bodyCollector{
		digest: sha256.New(), capture: capture, captureLimit: limit,
	}
}

func (c *bodyCollector) accept(body []byte) {
	_, _ = c.digest.Write(body)
	c.accepted += int64(len(body))
	if !c.capture {
		return
	}
	keep := len(body)
	if c.captureLimit >= 0 {
		remaining := c.captureLimit - int64(len(c.body))
		if remaining <= 0 {
			return
		}
		if int64(keep) > remaining {
			keep = int(remaining)
		}
	}
	c.body = append(c.body, body[:keep]...)
}

func (c *bodyCollector) sum() [sha256.Size]byte {
	var result [sha256.Size]byte
	copy(result[:], c.digest.Sum(nil))
	return result
}

func (c *bodyCollector) capturedBody() []byte {
	if !c.capture {
		return nil
	}
	return append([]byte{}, c.body...)
}
