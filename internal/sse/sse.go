// Package sse reads and writes Server-Sent Event frames.
//
// Two directions, and they are not symmetric. Reading is against a provider's
// output, so it follows the SSE specification rather than any one provider's
// habits: multiple `data:` lines in one event concatenate, comment lines are
// ignored, and a trailing event without a blank line still counts. Writing is
// Pensara's own transport to the Oxy edge, so it is deliberately minimal — one
// named frame per message and nothing else.
//
// There is no `[DONE]` sentinel on the way out. The contract's `done` and
// `error` events are already terminal, and a second terminality signal is a
// second thing that can disagree with the first.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxEventBytes bounds one decoded event. A provider that never emits a blank
// line would otherwise grow this buffer until the process dies, which is a
// denial of service that arrives looking like a memory leak.
const maxEventBytes = 8 << 20

// Event is one decoded frame.
type Event struct {
	// Name is the `event:` field, empty when the producer sent none.
	Name string
	// Data is the concatenation of the frame's `data:` lines, newline-joined.
	Data string
}

// Decoder reads SSE frames from an upstream response body.
type Decoder struct {
	scanner *bufio.Scanner
	err     error
}

// NewDecoder reads frames from r.
func NewDecoder(r io.Reader) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	return &Decoder{scanner: scanner}
}

// Next returns the next frame, or false when the stream ends.
//
// A frame the producer left unterminated (no trailing blank line before EOF) is
// still returned: an upstream that is cut off mid-stream has usually already
// sent output worth counting, and discarding it would lose usage that a partial
// settlement depends on.
func (d *Decoder) Next() (Event, bool) {
	var (
		name string
		data []string
	)
	for d.scanner.Scan() {
		line := strings.TrimSuffix(d.scanner.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 || name != "" {
				return Event{Name: name, Data: strings.Join(data, "\n")}, true
			}
		case strings.HasPrefix(line, ":"):
			// A comment, commonly used as a keep-alive. Not an event.
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		default:
			// `id:`, `retry:` and unknown fields carry nothing Pensara reads.
		}
	}
	if err := d.scanner.Err(); err != nil {
		d.err = err
	}
	if len(data) > 0 || name != "" {
		return Event{Name: name, Data: strings.Join(data, "\n")}, true
	}
	return Event{}, false
}

// Err reports why the stream ended, if it ended in failure. A context
// cancellation surfaces here, which is how a cancelled read is distinguished
// from an upstream that simply finished.
func (d *Decoder) Err() error { return d.err }

// ErrClientGone is returned by Writer once the downstream connection has failed.
// It is not an execution error: the caller uses it to stop the upstream call,
// which is the whole cancellation path.
var ErrClientGone = errors.New("sse: the downstream client is gone")

// Writer emits named frames to the Oxy edge and flushes each one.
//
// Flushing per frame is the difference between streaming and buffering. Without
// it Go's ResponseWriter accumulates until its buffer fills, which turns
// time-to-first-token into time-to-last-token and is invisible in any test that
// only reads the completed response.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
	broken  bool
}

// NewWriter prepares w for streaming and writes the SSE response headers.
//
// It fails rather than degrading if the ResponseWriter cannot flush: a
// non-flushable writer streams nothing, and discovering that as "the customer
// saw one big response at the end" is worse than refusing at the start.
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("sse: %T cannot flush, so it cannot stream", w)
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-store")
	header.Set("Connection", "keep-alive")
	// Defence in depth against a reverse proxy that buffers by default and
	// would otherwise silently undo everything above.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &Writer{w: w, flusher: flusher}, nil
}

// WriteEvent emits one named frame carrying payload.
//
// The name is Pensara's own transport framing, not part of the published
// contract: the contract specifies the shapes exchanged and says nothing about
// how they are carried. Naming the frames is what lets one connection carry both
// the customer-visible event stream and the technical usage record without a
// consumer having to guess which shape it just received from the shape itself.
func (w *Writer) WriteEvent(name string, payload []byte) error {
	if w.broken {
		return ErrClientGone
	}
	if strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("sse: event name %q contains a line break, which would forge a frame boundary", name)
	}
	var frame bytes.Buffer
	frame.Grow(len(payload) + len(name) + 16)
	frame.WriteString("event: ")
	frame.WriteString(name)
	frame.WriteString("\ndata: ")
	frame.Write(payload)
	frame.WriteString("\n\n")
	if _, err := w.w.Write(frame.Bytes()); err != nil {
		w.broken = true
		return fmt.Errorf("%w: %w", ErrClientGone, err)
	}
	w.flusher.Flush()
	return nil
}
