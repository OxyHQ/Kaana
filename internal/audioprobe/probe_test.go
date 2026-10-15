package audioprobe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

// The fixtures in testdata are one 1.5 s 440 Hz sine, mono, written by
// ffmpeg 7 in every container a transcription upload may use:
//
//	S="-f lavfi -i sine=frequency=440:sample_rate=16000:duration=1.5 -ac 1 -map_metadata -1 -fflags +bitexact -flags:a +bitexact"
//	ffmpeg $S -c:a pcm_mulaw -ar 8000 clip-mulaw.wav
//	ffmpeg $S -c:a libmp3lame -b:a 16k -ar 16000 clip.mp3
//	ffmpeg $S -c:a flac -ar 8000 -sample_fmt s16 clip.flac
//	ffmpeg $S -c:a libopus -b:a 6k clip-opus.ogg
//	ffmpeg $S -c:a libvorbis -q:a 0 -ar 16000 clip-vorbis.ogg
//	ffmpeg $S -c:a libopus -b:a 6k -live 1 -f webm clip-mediarecorder.webm
//	ffmpeg $S -c:a aac -b:a 16k clip.m4a
//	ffmpeg $S -c:a aac -b:a 16k -movflags frag_keyframe+empty_moov+default_base_moof -f mp4 clip-fragmented.m4a
//	ffmpeg $S -c:a aac -b:a 16k -f adts clip.aac
//
// Each expectation is what the container's own timing says, checked against
// ffprobe (`-count_packets -show_entries stream=duration_ts,time_base,nb_read_packets`):
//
//	clip-mulaw.wav          12 000 samples at 8 kHz                          1500
//	clip.flac               STREAMINFO 12 000 samples at 8 kHz                1500
//	clip.mp3                44 frames × 576 at 16 kHz (the Info frame not
//	                        counted; ffprobe's 44 packets)                    1584
//	clip.aac                25 ADTS frames × 1024 at 16 kHz                   1600
//	clip-opus.ogg           last granule 72 312 − pre-skip 312 at 48 kHz      1500
//	clip-vorbis.ogg         last granule 24 000 at 16 kHz                     1500
//	clip.m4a                stts 25 024 ticks at 16 kHz (ffprobe's
//	                        fragmented reading; its 24 000 for the
//	                        unfragmented file applies the edit list's
//	                        priming, which this probe does not subtract)    1564
//	clip-fragmented.m4a     trun 25 024 ticks at 16 kHz, header duration 0    1564
//	clip-mediarecorder.webm no Duration element (ffprobe: N/A); 76 Opus
//	                        blocks of 20 ms by their TOC, timestamps in ms    1521
func TestTheContainersOwnTimingIsRead(t *testing.T) {
	for _, tc := range []struct {
		file, mediaType string
		want            int
	}{
		{"clip-mulaw.wav", "audio/wav", 1500},
		{"clip.flac", "audio/flac", 1500},
		{"clip.mp3", "audio/mpeg", 1584},
		{"clip.aac", "audio/aac", 1600},
		{"clip-opus.ogg", "audio/ogg", 1500},
		{"clip-vorbis.ogg", "audio/ogg", 1500},
		{"clip.m4a", "audio/mp4", 1564},
		{"clip-fragmented.m4a", "audio/mp4", 1564},
		{"clip-mediarecorder.webm", "audio/webm", 1521},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Milliseconds(tc.mediaType, data)
			if err != nil || got != tc.want {
				t.Fatalf("Milliseconds = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// wav builds a RIFF/WAVE file: a fmt chunk and a data chunk of dataBytes
// bytes, whose header may state a different size.
func wav(format, channels uint16, rate uint32, bitsPerSample uint16, dataBytes int, statedData uint32, extra ...[]byte) []byte {
	var b bytes.Buffer
	put := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	put(uint32(0))
	b.WriteString("WAVEfmt ")
	put(uint32(16))
	blockAlign := channels * bitsPerSample / 8
	put(format)
	put(channels)
	put(rate)
	put(rate * uint32(blockAlign))
	put(blockAlign)
	put(bitsPerSample)
	for _, chunk := range extra {
		b.Write(chunk)
	}
	b.WriteString("data")
	put(statedData)
	b.Write(make([]byte, dataBytes))
	return b.Bytes()
}

func TestAWAVIsItsSampleFramesOverItsRate(t *testing.T) {
	// 24 kHz stereo 16-bit: 4 bytes a frame; 36 001 bytes is 9000 frames and a
	// partial one, 375 ms.
	if got, err := Milliseconds("audio/wav", wav(1, 2, 24_000, 16, 36_001, 36_001)); err != nil || got != 375 {
		t.Errorf("PCM = %d, %v", got, err)
	}
	// A streamed WAV whose data chunk claims 4 GiB: only the bytes present play.
	if got, err := Milliseconds("audio/wav", wav(1, 1, 16_000, 16, 32_000, 0xffffffff)); err != nil || got != 1000 {
		t.Errorf("a placeholder data size = %d, %v", got, err)
	}
	// A rounding case: one sample at 44.1 kHz is 0.0227 ms, rounded up to 1.
	if got, err := Milliseconds("audio/wav", wav(1, 1, 44_100, 16, 2, 2)); err != nil || got != 1 {
		t.Errorf("one sample = %d, %v", got, err)
	}
	// A compressed WAV (GSM 6.10) states its length only in a fact chunk.
	fact := []byte{'f', 'a', 'c', 't', 4, 0, 0, 0, 0x40, 0x1f, 0, 0} // 8000 samples
	if got, err := Milliseconds("audio/wav", wav(0x31, 1, 8_000, 0, 1625, 1625, fact)); err != nil || got != 1000 {
		t.Errorf("a compressed WAV with fact = %d, %v", got, err)
	}
	if _, err := Milliseconds("audio/wav", wav(0x31, 1, 8_000, 0, 1625, 1625)); !errors.Is(err, ErrUnmeasurable) {
		t.Errorf("a compressed WAV without fact = %v; it states no sample count", err)
	}
}

func TestWhatCannotBeMeasuredIsRefused(t *testing.T) {
	read := func(name string) []byte {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	mp3 := read("clip.mp3")
	broken := append(append([]byte{}, mp3[:2000]...), bytes.Repeat([]byte{0x55}, 300)...)
	broken = append(broken, mp3[2000:]...)
	fragmented := read("clip-fragmented.m4a")
	for name, tc := range map[string]struct {
		mediaType string
		data      []byte
	}{
		"raw PCM, which names no rate":          {"audio/pcm", make([]byte, 48_000)},
		"a media type a transcription refuses":  {"audio/x-unknown", mp3},
		"an MP3 labelled as WAV":                {"audio/wav", mp3},
		"a WebM labelled as MP4":                {"audio/mp4", read("clip-mediarecorder.webm")},
		"an Ogg labelled as WebM":               {"audio/webm", read("clip-opus.ogg")},
		"an MP3 that loses frame sync":          {"audio/mpeg", broken},
		"an empty upload":                       {"audio/mpeg", nil},
		"a FLAC truncated before STREAMINFO":    {"audio/flac", read("clip.flac")[:20]},
		"an Ogg page cut short":                 {"audio/ogg", read("clip-vorbis.ogg")[:100]},
		"an MP4 whose boxes overrun":            {"audio/mp4", fragmented[:len(fragmented)-10]},
		"an ADTS stream with a truncated frame": {"audio/aac", read("clip.aac")[:3]},
		"a WAV with no data chunk":              {"audio/wav", wav(1, 1, 8000, 16, 0, 0)[:36]},
	} {
		if got, err := Milliseconds(tc.mediaType, tc.data); !errors.Is(err, ErrUnmeasurable) {
			t.Errorf("%s: = %d, %v; want a refusal", name, got, err)
		}
	}
	// Control: the unbroken MP3 is measured.
	if _, err := Milliseconds("audio/mpeg", mp3); err != nil {
		t.Errorf("control: %v", err)
	}
}

// TestADurationPastADayIsRefused: a container can claim any timing; one past
// MaxMilliseconds is not a duration a hold is sized from.
func TestADurationPastADayIsRefused(t *testing.T) {
	data, err := os.ReadFile("testdata/clip.flac")
	if err != nil {
		t.Fatal(err)
	}
	lying := append([]byte{}, data...)
	// STREAMINFO total samples: 2^36 − 1 at 8 kHz is about 99 days.
	lying[8+13] |= 0x0f
	copy(lying[8+14:8+18], []byte{0xff, 0xff, 0xff, 0xff})
	if _, err := Milliseconds("audio/flac", lying); !errors.Is(err, ErrUnmeasurable) {
		t.Errorf("a 99-day FLAC = %v", err)
	}
}

func TestOpusPacketDurationsFollowTheTOC(t *testing.T) {
	for _, tc := range []struct {
		packet []byte
		want   uint64
		ok     bool
	}{
		{[]byte{0x08}, 20_000_000, true},         // SILK NB 20 ms, one frame
		{[]byte{0x19}, 120_000_000, true},        // SILK NB 60 ms, code 1: two frames
		{[]byte{0xfc}, 20_000_000, true},         // CELT FB 20 ms, one frame
		{[]byte{0xe3, 0x03}, 7_500_000, true},    // CELT FB 2.5 ms, code 3, three frames
		{[]byte{0x1b, 0x03}, 180_000_000, false}, // SILK 60 ms × 3: past Opus's 120 ms
		{[]byte{0x03}, 0, false},                 // code 3 without its count
		{nil, 0, false},
	} {
		got, ok := opusPacketDuration(tc.packet)
		if ok != tc.ok || ok && got != tc.want {
			t.Errorf("opusPacketDuration(%x) = %d, %t; want %d, %t", tc.packet, got, ok, tc.want, tc.ok)
		}
	}
}
