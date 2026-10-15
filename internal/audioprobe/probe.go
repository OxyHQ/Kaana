// Package audioprobe measures how long an uploaded audio file plays, from its
// own container, before anything is sent to a provider.
//
// A transcription is billed by the duration of its audio, and Oxy sizes the
// hold for one from the duration Kaana reports in the grant request
// (`audioInputMilliseconds`, Kaana ADR 0002 decision 3; Alia ADR 0017 §7).
// The byte count says nothing about it — a 1 MiB file is two seconds of WAV
// or an hour of 2 kbit/s Opus — so the duration is READ, from the timing the
// container itself carries, and an upload whose duration cannot be read is
// refused rather than estimated:
//
//	audio/wav   the data chunk over the fmt chunk's block alignment (PCM,
//	            IEEE float, G.711), or the fact chunk's sample count
//	audio/flac  STREAMINFO's total samples over its sample rate
//	audio/mpeg  every MPEG audio frame, counted (an Xing/Info/VBRI header
//	            frame is metadata and is not counted)
//	audio/aac   every ADTS frame, counted
//	audio/ogg   the logical stream's last granule position (Opus minus its
//	            pre-skip at 48 kHz, Vorbis and FLAC at their own rate)
//	audio/webm  the audio track's blocks: the latest block's timestamp plus
//	            its duration, from BlockDuration, the track's
//	            DefaultDuration or, for Opus, the packet's own TOC — so the
//	            files a browser's MediaRecorder writes, which carry no
//	            Duration element, are measured too
//	audio/mp4   the sound track's sample table (stts) and every movie
//	            fragment's run (trun), over the track's timescale — so a
//	            fragmented file whose header duration is zero is measured too
//	audio/pcm   refused: raw samples carry no rate, and the media type names
//	            none
//
// The result is milliseconds, rounded up once. It is a measurement of the
// file's own statement of its timing, not a decode: a container that lies
// about its timing is the caller's own file, and settlement still charges
// the provider's measurement of the audio it transcribed.
package audioprobe

import (
	"errors"
	"fmt"
	"math/bits"
)

// ErrUnmeasurable is every refusal: the upload's duration cannot be read
// from its container. errors.Is matches it; the message says why.
var ErrUnmeasurable = errors.New("audioprobe: the upload's duration cannot be read from its container")

func refuse(mediaType, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrUnmeasurable, mediaType, fmt.Sprintf(format, args...))
}

// MaxMilliseconds bounds a measured duration: a day of audio, far past what
// 20 MiB can carry at any rate a transcription takes. A container claiming
// more is refused as unreadable rather than sizing a hold from it.
const MaxMilliseconds = 86_400_000

// Milliseconds is how long the upload plays, in milliseconds rounded up.
// mediaType is one of the contract's `audioInputMediaTypeSchema` members
// (without parameters); data is the whole upload.
func Milliseconds(mediaType string, data []byte) (int, error) {
	var (
		units, perSecond uint64
		err              error
	)
	switch mediaType {
	case "audio/wav":
		units, perSecond, err = wavDuration(data)
	case "audio/flac":
		units, perSecond, err = flacDuration(skipID3v2(data))
	case "audio/mpeg":
		units, perSecond, err = mpegDuration(data)
	case "audio/aac":
		units, perSecond, err = adtsDuration(skipID3v2(data))
	case "audio/ogg":
		units, perSecond, err = oggDuration(data)
	case "audio/webm":
		units, perSecond, err = matroskaDuration(data)
	case "audio/mp4":
		units, perSecond, err = mp4Duration(data)
	case "audio/pcm":
		return 0, refuse(mediaType, "raw PCM carries no sample rate or channel count, and the media type names none")
	default:
		return 0, refuse(mediaType, "not an audio media type a transcription takes")
	}
	if err != nil {
		return 0, refuse(mediaType, "%s", err.Error())
	}
	if perSecond == 0 {
		return 0, refuse(mediaType, "the container states no rate")
	}
	milliseconds, ok := ceilMillis(units, perSecond)
	if !ok || milliseconds > MaxMilliseconds {
		return 0, refuse(mediaType, "the container states a duration past %d ms", MaxMilliseconds)
	}
	return int(milliseconds), nil
}

// ceilMillis is ⌈units × 1000 / perSecond⌉ without overflow.
func ceilMillis(units, perSecond uint64) (uint64, bool) {
	high, low := bits.Mul64(units, 1000)
	if high >= perSecond {
		return 0, false
	}
	quotient, remainder := bits.Div64(high, low, perSecond)
	if remainder > 0 {
		quotient++
	}
	return quotient, true
}

// skipID3v2 drops a leading ID3v2 tag ("ID3", version, flags, a 28-bit
// synchsafe size, and a 10-byte footer when flagged), which MP3, AAC and
// FLAC files may begin with.
func skipID3v2(data []byte) []byte {
	if len(data) < 10 || string(data[:3]) != "ID3" {
		return data
	}
	size := int(data[6]&0x7f)<<21 | int(data[7]&0x7f)<<14 | int(data[8]&0x7f)<<7 | int(data[9]&0x7f)
	end := 10 + size
	if data[5]&0x10 != 0 {
		end += 10
	}
	if end > len(data) {
		return nil
	}
	return data[end:]
}

func be16(b []byte) uint32 { return uint32(b[0])<<8 | uint32(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64(b []byte) uint64 { return uint64(be32(b))<<32 | uint64(be32(b[4:])) }
func le16(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func le64(b []byte) uint64 { return uint64(le32(b)) | uint64(le32(b[4:]))<<32 }
