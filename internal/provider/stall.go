package provider

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultStreamIdleTimeout bounds how long a stream that has already answered
// its headers may go without sending one real frame.
//
// Keep-alive comments are not frames: OpenRouter sends them for as long as it
// waits on a model, which is exactly the state this bounds. The value is
// generous on purpose. A reasoning model may think for minutes before its
// first visible frame, and cutting that short would fail a request that was
// going to succeed; an upstream that has said nothing for five minutes has
// stalled.
const DefaultStreamIdleTimeout = 300 * time.Second

var errStreamStalled = errors.New("provider: the stream sent nothing for longer than its idle timeout")

// StreamWatch cancels a stream's context when it goes idle for longer than a
// bound. Arm starts the clock once the response headers are in; Progress resets
// it on every real frame; Stalled reports whether the watch is what ended the
// stream, so the adapter can report a timeout rather than a transport error.
type StreamWatch struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   time.Duration

	mutex sync.Mutex
	timer *time.Timer
}

// WatchStream derives the context a stream's request and body read must use.
// A non-positive idle takes DefaultStreamIdleTimeout.
func WatchStream(ctx context.Context, idle time.Duration) (context.Context, *StreamWatch) {
	if idle <= 0 {
		idle = DefaultStreamIdleTimeout
	}
	derived, cancel := context.WithCancelCause(ctx)
	return derived, &StreamWatch{ctx: derived, cancel: cancel, idle: idle}
}

// Arm starts the idle clock.
func (w *StreamWatch) Arm() {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.timer == nil {
		w.timer = time.AfterFunc(w.idle, func() { w.cancel(errStreamStalled) })
	}
}

// Progress records one real frame.
func (w *StreamWatch) Progress() {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if w.timer != nil {
		w.timer.Reset(w.idle)
	}
}

// Stalled reports whether the idle bound ended the stream.
func (w *StreamWatch) Stalled() bool {
	return errors.Is(context.Cause(w.ctx), errStreamStalled)
}

// Idle is the bound in force.
func (w *StreamWatch) Idle() time.Duration { return w.idle }

// Stop releases the watch. It must run when the stream is done.
func (w *StreamWatch) Stop() {
	w.mutex.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mutex.Unlock()
	w.cancel(context.Canceled)
}
