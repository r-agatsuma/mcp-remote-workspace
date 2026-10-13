package workspace

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// streamCapture drains arbitrary output while retaining a fixed head/tail ring.
// The UTF-8 scanner remembers at most one incomplete rune across writes.
type streamCapture struct {
	mu         sync.Mutex
	limit      int
	head, tail []byte
	position   int
	total      uint64
	invalid    bool
	pending    []byte
}

func newStreamCapture(limit int) *streamCapture {
	return &streamCapture{limit: limit, head: make([]byte, 0, (limit+1)/2), tail: make([]byte, 0, limit/2)}
}

func (b *streamCapture) scan(p []byte) {
	if len(b.pending) > 0 {
		for len(p) > 0 && !utf8.FullRune(b.pending) {
			b.pending = append(b.pending, p[0])
			p = p[1:]
		}
		if !utf8.FullRune(b.pending) {
			return
		}
		_, n := utf8.DecodeRune(b.pending)
		if n == 1 {
			b.invalid = true
		}
		// Invalid prefixes can leave bytes which belong to the next rune.
		remaining := append([]byte(nil), b.pending[n:]...)
		b.pending = b.pending[:0]
		if len(remaining) > 0 {
			b.scan(remaining)
		}
		if len(b.pending) > 0 {
			b.scan(p)
			return
		}
	}
	for len(p) > 0 {
		if !utf8.FullRune(p) {
			b.pending = append(b.pending[:0], p...)
			return
		}
		_, n := utf8.DecodeRune(p)
		if n == 1 && p[0] >= utf8.RuneSelf {
			b.invalid = true
		}
		p = p[n:]
	}
}

func (b *streamCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += uint64(n)
	b.scan(p)
	if remaining := cap(b.head) - len(b.head); remaining > 0 {
		take := min(remaining, len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) >= cap(b.tail) {
		b.tail = append(b.tail[:0], p[len(p)-cap(b.tail):]...)
		b.position = 0
	} else {
		if remaining := cap(b.tail) - len(b.tail); remaining > 0 {
			take := min(remaining, len(p))
			b.tail = append(b.tail, p[:take]...)
			p = p[take:]
		}
		if len(p) > 0 {
			take := min(len(p), len(b.tail)-b.position)
			copy(b.tail[b.position:], p[:take])
			copy(b.tail, p[take:])
			b.position = (b.position + len(p)) % len(b.tail)
		}
	}
	return n, nil
}

func (b *streamCapture) bytesLocked() []byte {
	data := make([]byte, 0, len(b.head)+len(b.tail))
	data = append(data, b.head...)
	data = append(data, b.tail[b.position:]...)
	return append(data, b.tail[:b.position]...)
}
func (b *streamCapture) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytesLocked()
}
func (b *streamCapture) totalBytes() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
func (b *streamCapture) result() StreamResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	// ASCII '?' replacement cannot expand the retained byte budget, even when
	// every output byte is invalid. No raw encoding error is reflected.
	return StreamResult{Text: strings.ToValidUTF8(string(b.bytesLocked()), "?"), TotalBytes: b.total,
		Truncated: b.total > uint64(b.limit), InvalidUTF8: b.invalid || len(b.pending) > 0}
}
