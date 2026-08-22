package sse_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/sse"
)

func TestDecoderFollowsTheSpecificationRatherThanOneProvidersHabits(t *testing.T) {
	stream := strings.Join([]string{
		": a keep-alive comment",
		"",
		"data: {\"a\":1}",
		"",
		"event: usage_report",
		"data: {\"b\":",
		"data: 2}",
		"",
		"id: 7",
		"retry: 1000",
		"data: {\"c\":3}",
		"",
		// Deliberately unterminated: an upstream cut off mid-stream has usually
		// already sent output worth counting.
		"data: {\"d\":4}",
	}, "\n")

	decoder := sse.NewDecoder(strings.NewReader(stream))
	var got []sse.Event
	for {
		event, more := decoder.Next()
		if !more {
			break
		}
		got = append(got, event)
	}
	if decoder.Err() != nil {
		t.Fatalf("decoding: %v", decoder.Err())
	}

	want := []sse.Event{
		{Data: `{"a":1}`},
		{Name: "usage_report", Data: "{\"b\":\n2}"},
		{Data: `{"c":3}`},
		{Data: `{"d":4}`},
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d frames, expected %d: %#v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("frame %d is %#v, expected %#v", index, got[index], want[index])
		}
	}
}

func TestWriterEmitsNamedFlushedFrames(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer, err := sse.NewWriter(recorder)
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	if err := writer.WriteEvent("stream_event", []byte(`{"type":"start"}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := writer.WriteEvent("usage_report", []byte(`{"outcome":"completed"}`)); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type is %q", got)
	}
	// A reverse proxy that buffers would silently undo the flushing above.
	if got := recorder.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering is %q", got)
	}
	want := "event: stream_event\ndata: {\"type\":\"start\"}\n\nevent: usage_report\ndata: {\"outcome\":\"completed\"}\n\n"
	if got := recorder.Body.String(); got != want {
		t.Errorf("wrote:\n%q\nexpected:\n%q", got, want)
	}
}

// TestWriterRefusesANameThatWouldForgeAFrameBoundary guards the one way a
// caller could inject a frame: a name containing a line break ends the event
// line early and everything after it is read as new fields.
func TestWriterRefusesANameThatWouldForgeAFrameBoundary(t *testing.T) {
	writer, err := sse.NewWriter(httptest.NewRecorder())
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	if err := writer.WriteEvent("stream_event\ndata: injected", []byte(`{}`)); err == nil {
		t.Fatal("a name containing a line break was accepted")
	}
}

func TestWriterReportsAGoneClient(t *testing.T) {
	writer, err := sse.NewWriter(&brokenResponseWriter{header: http.Header{}})
	if err != nil {
		t.Fatalf("building the writer: %v", err)
	}
	err = writer.WriteEvent("stream_event", []byte(`{}`))
	if !errors.Is(err, sse.ErrClientGone) {
		t.Fatalf("a failed write reported %v, expected ErrClientGone", err)
	}
	// Once broken it stays broken, so a caller cannot keep streaming into a
	// connection that is gone.
	if err := writer.WriteEvent("stream_event", []byte(`{}`)); !errors.Is(err, sse.ErrClientGone) {
		t.Fatalf("a second write after a failure reported %v", err)
	}
}

func TestWriterRefusesAResponseWriterThatCannotFlush(t *testing.T) {
	// A non-flushable writer streams nothing, and discovering that as "the
	// customer saw one response at the end" is worse than refusing at the start.
	if _, err := sse.NewWriter(&unflushableResponseWriter{header: http.Header{}}); err == nil {
		t.Fatal("a writer that cannot flush was accepted")
	}
}

type brokenResponseWriter struct{ header http.Header }

func (b *brokenResponseWriter) Header() http.Header { return b.header }
func (b *brokenResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("connection reset by peer")
}
func (b *brokenResponseWriter) WriteHeader(int) {}
func (b *brokenResponseWriter) Flush()          {}

type unflushableResponseWriter struct{ header http.Header }

func (u *unflushableResponseWriter) Header() http.Header         { return u.header }
func (u *unflushableResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (u *unflushableResponseWriter) WriteHeader(int)             {}
