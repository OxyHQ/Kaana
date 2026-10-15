package audioprobe

import (
	"errors"
	"fmt"
	"math/bits"
)

// Matroska/WebM element ids (with their length marker, as the spec writes
// them).
const (
	idEBML            = 0x1a45dfa3
	idDocType         = 0x4282
	idSegment         = 0x18538067
	idInfo            = 0x1549a966
	idTimecodeScale   = 0x2ad7b1
	idTracks          = 0x1654ae6b
	idTrackEntry      = 0xae
	idTrackNumber     = 0xd7
	idTrackType       = 0x83
	idCodecID         = 0x86
	idDefaultDuration = 0x23e383
	idCluster         = 0x1f43b675
	idTimecode        = 0xe7
	idSimpleBlock     = 0xa3
	idBlockGroup      = 0xa0
	idBlock           = 0xa1
	idBlockDuration   = 0x9b
)

// defaultTimecodeScale is Matroska's default: one tick is a millisecond.
const defaultTimecodeScale = 1_000_000

// unknownSize marks an element whose size the muxer did not know when it
// wrote it — a live stream, and every Segment and Cluster a browser's
// MediaRecorder writes. Such an element runs until the next element that
// cannot be its child.
const unknownSize = ^uint64(0)

type element struct {
	id         uint32
	size       uint64
	headerSize int
}

// readElement reads one element header: a variable-length id that keeps its
// marker, and a variable-length size that does not.
func readElement(data []byte) (element, bool) {
	id, idLength, ok := readVint(data, true)
	if !ok || idLength > 4 {
		return element{}, false
	}
	size, sizeLength, ok := readVint(data[idLength:], false)
	if !ok {
		return element{}, false
	}
	if size == (uint64(1)<<(7*sizeLength))-1 {
		size = unknownSize
	}
	return element{id: uint32(id), size: size, headerSize: idLength + sizeLength}, true
}

func readVint(data []byte, keepMarker bool) (uint64, int, bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false
	}
	length := bits.LeadingZeros8(data[0]) + 1
	if length > 8 || len(data) < length {
		return 0, 0, false
	}
	value := uint64(data[0])
	if !keepMarker {
		value &= 0xff >> length
	}
	for _, b := range data[1:length] {
		value = value<<8 | uint64(b)
	}
	return value, length, true
}

func readUint(body []byte) uint64 {
	var value uint64
	for _, b := range body {
		value = value<<8 | uint64(b)
	}
	return value
}

type audioTrack struct {
	number          uint64
	codec           string
	defaultDuration uint64 // nanoseconds per frame; 0 if absent
}

// matroskaReader walks a Matroska/WebM file. Segment, Tracks and Cluster are
// read in place (their children follow them in the stream), so a Segment or
// a Cluster of unknown size needs no end; every other element is read as a
// unit or skipped by its size.
type matroskaReader struct {
	scale     uint64
	tracks    []audioTrack
	audio     *audioTrack
	cluster   uint64 // the current cluster's timecode, in ticks
	inCluster bool
	start     uint64 // earliest block start, ns
	end       uint64 // latest block end, ns
	blocks    int
}

// matroskaDuration is the span of the one audio track's blocks: from the
// earliest block's timestamp to the latest block's timestamp plus its
// duration, in nanoseconds. A block's duration is its BlockDuration, else the
// track's DefaultDuration, else — for Opus — what its packet's TOC byte says
// (RFC 6716 §3.1). The Segment's Duration element is not read: the blocks are
// what plays, and the files a browser writes carry none.
func matroskaDuration(data []byte) (uint64, uint64, error) {
	header, ok := readElement(data)
	if !ok || header.id != idEBML || header.size == unknownSize || uint64(header.headerSize)+header.size > uint64(len(data)) {
		return 0, 0, errors.New("not an EBML document")
	}
	headerBody := data[header.headerSize : uint64(header.headerSize)+header.size]
	docType := ""
	if err := eachChild(headerBody, func(child element, body []byte) error {
		if child.id == idDocType {
			docType = string(body)
		}
		return nil
	}); err != nil {
		return 0, 0, err
	}
	if docType != "webm" && docType != "matroska" {
		return 0, 0, fmt.Errorf("the EBML document is %q, not WebM or Matroska", docType)
	}
	reader := &matroskaReader{scale: defaultTimecodeScale}
	if err := reader.walk(data[uint64(header.headerSize)+header.size:]); err != nil {
		return 0, 0, err
	}
	if reader.blocks == 0 {
		return 0, 0, errors.New("no audio block")
	}
	return reader.end - reader.start, 1_000_000_000, nil
}

func (r *matroskaReader) walk(data []byte) error {
	for offset := 0; offset < len(data); {
		current, ok := readElement(data[offset:])
		if !ok {
			return fmt.Errorf("no element at byte %d", offset)
		}
		bodyStart := offset + current.headerSize
		switch current.id {
		case idSegment, idTracks, idCluster:
			// Read in place: the children follow.
			if current.id == idCluster {
				r.inCluster, r.cluster = false, 0
			}
			offset = bodyStart
			continue
		}
		if current.size == unknownSize || uint64(bodyStart)+current.size > uint64(len(data)) {
			return fmt.Errorf("element %#x at byte %d overruns the file", current.id, offset)
		}
		body := data[bodyStart : uint64(bodyStart)+current.size]
		if err := r.element(current, body); err != nil {
			return err
		}
		offset = int(uint64(bodyStart) + current.size)
	}
	return nil
}

func (r *matroskaReader) element(current element, body []byte) error {
	switch current.id {
	case idInfo:
		return eachChild(body, func(child element, value []byte) error {
			if child.id == idTimecodeScale {
				r.scale = readUint(value)
			}
			return nil
		})
	case idTrackEntry:
		return r.trackEntry(body)
	case idTimecode:
		r.cluster, r.inCluster = readUint(body), true
	case idSimpleBlock:
		return r.block(body, 0, false)
	case idBlockGroup:
		var (
			block       []byte
			duration    uint64
			hasDuration bool
		)
		if err := eachChild(body, func(child element, value []byte) error {
			switch child.id {
			case idBlock:
				block = value
			case idBlockDuration:
				duration, hasDuration = readUint(value), true
			}
			return nil
		}); err != nil {
			return err
		}
		if block == nil {
			return errors.New("a block group carries no block")
		}
		return r.block(block, duration, hasDuration)
	}
	return nil
}

func (r *matroskaReader) trackEntry(body []byte) error {
	var (
		track     audioTrack
		trackType uint64
	)
	if err := eachChild(body, func(child element, value []byte) error {
		switch child.id {
		case idTrackNumber:
			track.number = readUint(value)
		case idTrackType:
			trackType = readUint(value)
		case idCodecID:
			track.codec = string(value)
		case idDefaultDuration:
			track.defaultDuration = readUint(value)
		}
		return nil
	}); err != nil {
		return err
	}
	if trackType != 2 { // audio
		return nil
	}
	if r.audio != nil {
		return errors.New("more than one audio track")
	}
	r.tracks = append(r.tracks, track)
	r.audio = &r.tracks[len(r.tracks)-1]
	return nil
}

// block reads one (Simple)Block of the audio track: its track number, its
// signed timecode relative to the cluster's, its lacing and its frames.
func (r *matroskaReader) block(body []byte, groupDuration uint64, hasGroupDuration bool) error {
	if r.audio == nil {
		return errors.New("a block precedes the audio track's definition")
	}
	track, trackLength, ok := readVint(body, false)
	if !ok || len(body) < trackLength+3 {
		return errors.New("an unreadable block header")
	}
	if track != r.audio.number {
		return nil
	}
	if !r.inCluster {
		return errors.New("a block outside a cluster with a timecode")
	}
	relative := int64(int16(be16(body[trackLength:])))
	lacing := body[trackLength+2] >> 1 & 0x03
	frames := body[trackLength+3:]
	if r.scale == 0 {
		return errors.New("a timecode scale of zero")
	}
	ticks := int64(r.cluster) + relative
	if ticks < 0 {
		return errors.New("a block before the start of time")
	}
	high, start := bits.Mul64(uint64(ticks), r.scale)
	if high != 0 {
		return errors.New("a block timecode past what a duration can hold")
	}

	var duration uint64
	switch {
	case hasGroupDuration:
		high, duration = bits.Mul64(groupDuration, r.scale)
		if high != 0 {
			return errors.New("a block duration past what a duration can hold")
		}
	case lacing == 0 && r.audio.defaultDuration > 0:
		duration = r.audio.defaultDuration
	case lacing != 0 && r.audio.defaultDuration > 0 && len(frames) > 0:
		duration = r.audio.defaultDuration * (uint64(frames[0]) + 1)
	case lacing == 0 && r.audio.codec == "A_OPUS":
		var ok bool
		if duration, ok = opusPacketDuration(frames); !ok {
			return errors.New("an Opus packet whose TOC states no duration")
		}
	default:
		return fmt.Errorf("a %s block states no duration", r.audio.codec)
	}
	if r.blocks == 0 || start < r.start {
		r.start = start
	}
	r.end = max(r.end, start+duration)
	r.blocks++
	return nil
}

// eachChild visits the elements of a body whose size is known.
func eachChild(body []byte, visit func(element, []byte) error) error {
	for offset := 0; offset < len(body); {
		child, ok := readElement(body[offset:])
		if !ok || child.size == unknownSize || uint64(offset+child.headerSize)+child.size > uint64(len(body)) {
			return fmt.Errorf("an unreadable child element at byte %d", offset)
		}
		start := offset + child.headerSize
		if err := visit(child, body[start:uint64(start)+child.size]); err != nil {
			return err
		}
		offset = int(uint64(start) + child.size)
	}
	return nil
}

// opusPacketDuration is the duration one Opus packet decodes to, in
// nanoseconds, from its TOC byte (RFC 6716 §3.1): the configuration's frame
// size times the frame count its code states. At most 120 ms.
func opusPacketDuration(packet []byte) (uint64, bool) {
	if len(packet) == 0 {
		return 0, false
	}
	toc := packet[0]
	config := toc >> 3
	var frame uint64 // nanoseconds
	switch {
	case config < 12: // SILK
		frame = [4]uint64{10_000_000, 20_000_000, 40_000_000, 60_000_000}[config%4]
	case config < 16: // hybrid
		frame = [2]uint64{10_000_000, 20_000_000}[config%2]
	default: // CELT
		frame = [4]uint64{2_500_000, 5_000_000, 10_000_000, 20_000_000}[config%4]
	}
	count := uint64(1)
	switch toc & 0x03 {
	case 1, 2:
		count = 2
	case 3:
		if len(packet) < 2 || packet[1]&0x3f == 0 {
			return 0, false
		}
		count = uint64(packet[1] & 0x3f)
	}
	total := frame * count
	return total, total <= 120_000_000
}
