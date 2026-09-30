package realtime

// replay holds the most recent encoded events of one session, so a client
// that reconnects can be sent exactly the events after the last one it
// processed. It stores the bytes that were sent, so a replayed event is
// byte-identical to the original and recognisable by its sequence.
//
// It is bounded twice, by count and by bytes, and forgets the OLDEST events
// first. A resume that names a sequence it has forgotten is refused: nothing
// is guessed.
type replay struct {
	events   [][]byte
	first    int // sequence of events[0]
	bytes    int
	maxCount int
	maxBytes int
}

func newReplay(maxCount, maxBytes int) *replay {
	return &replay{maxCount: maxCount, maxBytes: maxBytes}
}

// push records the event numbered sequence, which is always the next one.
func (r *replay) push(sequence int, encoded []byte) {
	if len(r.events) == 0 {
		r.first = sequence
	}
	r.events = append(r.events, encoded)
	r.bytes += len(encoded)
	for len(r.events) > 1 && (len(r.events) > r.maxCount || r.bytes > r.maxBytes) {
		r.bytes -= len(r.events[0])
		r.events[0] = nil
		r.events = r.events[1:]
		r.first++
	}
}

// after returns every buffered event with a sequence greater than
// afterSequence, and false when an event the client has not seen is no longer
// buffered. next is the sequence the session will assign next.
func (r *replay) after(afterSequence, next int) ([][]byte, bool) {
	if afterSequence < -1 || afterSequence >= next {
		return nil, false
	}
	if afterSequence+1 == next {
		return nil, true
	}
	if len(r.events) == 0 || afterSequence+1 < r.first {
		return nil, false
	}
	return r.events[afterSequence+1-r.first:], true
}
