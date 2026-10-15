package audioprobe

import (
	"bytes"
	"errors"
	"fmt"
)

// MPEG audio (ISO/IEC 11172-3, 13818-3 and the MPEG 2.5 extension) frame
// tables: bitrates in kbit/s by [MPEG-1 or not][layer 1..3][index], and
// sample rates by [version bits][index].
var mpegBitrates = [2][3][16]uint32{
	{ // MPEG-1
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	},
	{ // MPEG-2 and 2.5
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	},
}

var mpegSampleRates = [4][3]uint32{
	{11025, 12000, 8000},  // MPEG 2.5
	{},                    // reserved
	{22050, 24000, 16000}, // MPEG-2
	{44100, 48000, 32000}, // MPEG-1
}

type mpegFrame struct {
	length  int
	samples uint64
	rate    uint32
	// headerOffset is where a Layer III Xing/Info header would start: after
	// the 4-byte header, the CRC if present and the side information.
	headerOffset int
	layer3       bool
}

// parseMPEGFrame reads one MPEG audio frame header. Free-format bitrates and
// every reserved value are refused: their frame length cannot be computed.
func parseMPEGFrame(header []byte) (mpegFrame, bool) {
	if len(header) < 4 || header[0] != 0xff || header[1]&0xe0 != 0xe0 {
		return mpegFrame{}, false
	}
	version := header[1] >> 3 & 3
	layer := 4 - int(header[1]>>1&3)
	bitrateIndex, rateIndex := header[2]>>4, header[2]>>2&3
	if version == 1 || layer == 4 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return mpegFrame{}, false
	}
	mpeg1 := version == 3
	table := 1
	if mpeg1 {
		table = 0
	}
	bitrate := uint64(mpegBitrates[table][layer-1][bitrateIndex]) * 1000
	rate := mpegSampleRates[version][rateIndex]
	padding := uint64(header[2] >> 1 & 1)
	frame := mpegFrame{rate: rate, layer3: layer == 3}
	switch {
	case layer == 1:
		frame.length, frame.samples = int((12*bitrate/uint64(rate)+padding)*4), 384
	case layer == 2 || mpeg1:
		frame.length, frame.samples = int(144*bitrate/uint64(rate)+padding), 1152
	default: // Layer III, MPEG-2 and 2.5
		frame.length, frame.samples = int(72*bitrate/uint64(rate)+padding), 576
	}
	mono := header[3]>>6 == 3
	sideInfo := 32
	switch {
	case mpeg1 && mono:
		sideInfo = 17
	case !mpeg1 && mono:
		sideInfo = 9
	case !mpeg1:
		sideInfo = 17
	}
	frame.headerOffset = 4 + sideInfo
	if header[1]&1 == 0 {
		frame.headerOffset += 2 // CRC
	}
	return frame, frame.length >= 4
}

// isVBRHeaderFrame is a Layer III frame carrying a Xing, Info or VBRI header:
// metadata an encoder writes in place of the first frame, decoded as silence
// and not part of the audio.
func isVBRHeaderFrame(frame mpegFrame, data []byte) bool {
	if !frame.layer3 {
		return false
	}
	at := frame.headerOffset
	if len(data) >= at+4 && (string(data[at:at+4]) == "Xing" || string(data[at:at+4]) == "Info") {
		return true
	}
	return len(data) >= 40 && string(data[36:40]) == "VBRI"
}

// trailingTag is metadata appended after the last frame: an ID3v1 tag, an
// APE tag or a Lyrics3 block.
func trailingTag(rest []byte) bool {
	return len(rest) == 128 && bytes.HasPrefix(rest, []byte("TAG")) ||
		bytes.HasPrefix(rest, []byte("APETAGEX")) || bytes.HasPrefix(rest, []byte("LYRICSBEGIN"))
}

// mpegDuration counts every frame of an MPEG audio stream. It does not trust
// a Xing/VBRI frame count: the frames present are what plays. A stream that
// loses frame sync, changes sample rate or carries no audio frame is refused.
func mpegDuration(data []byte) (uint64, uint64, error) {
	data = skipID3v2(data)
	var (
		samples uint64
		rate    uint32
		frames  int
	)
	for offset := 0; offset < len(data); {
		rest := data[offset:]
		if frames > 0 && trailingTag(rest) {
			break
		}
		frame, ok := parseMPEGFrame(rest)
		if !ok {
			if frames == 0 {
				return 0, 0, errors.New("no MPEG audio frame where the stream starts")
			}
			return 0, 0, fmt.Errorf("the stream loses frame sync at byte %d", offset)
		}
		if rate != 0 && frame.rate != rate {
			return 0, 0, errors.New("the sample rate changes mid-stream")
		}
		rate = frame.rate
		if frames > 0 || !isVBRHeaderFrame(frame, rest) {
			samples += frame.samples
		}
		frames++
		offset += frame.length
	}
	if samples == 0 {
		return 0, 0, errors.New("no audio frame")
	}
	return samples, uint64(rate), nil
}

// adtsSampleRates is the ADTS sampling_frequency_index table.
var adtsSampleRates = []uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// adtsDuration counts every ADTS frame of a raw AAC stream: 1024 samples per
// raw data block, one to four blocks per frame.
func adtsDuration(data []byte) (uint64, uint64, error) {
	var (
		samples uint64
		rate    uint32
		frames  int
	)
	for offset := 0; offset < len(data); {
		rest := data[offset:]
		if frames > 0 && trailingTag(rest) {
			break
		}
		if len(rest) < 7 || rest[0] != 0xff || rest[1]&0xf6 != 0xf0 {
			if frames == 0 {
				return 0, 0, errors.New("no ADTS frame where the stream starts")
			}
			return 0, 0, fmt.Errorf("the stream loses frame sync at byte %d", offset)
		}
		index := rest[2] >> 2 & 0x0f
		if int(index) >= len(adtsSampleRates) {
			return 0, 0, errors.New("a reserved sampling frequency index")
		}
		if rate != 0 && adtsSampleRates[index] != rate {
			return 0, 0, errors.New("the sample rate changes mid-stream")
		}
		rate = adtsSampleRates[index]
		length := int(rest[3]&0x03)<<11 | int(rest[4])<<3 | int(rest[5])>>5
		if length < 7 {
			return 0, 0, fmt.Errorf("a frame at byte %d states no length", offset)
		}
		samples += 1024 * (uint64(rest[6]&0x03) + 1)
		frames++
		offset += length
	}
	if samples == 0 {
		return 0, 0, errors.New("no audio frame")
	}
	return samples, uint64(rate), nil
}
