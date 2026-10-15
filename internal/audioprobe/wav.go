package audioprobe

import "errors"

// wavDuration reads a RIFF/WAVE file: sample frames over the sample rate.
//
// For the uncompressed formats a transcription upload carries — PCM (1),
// IEEE float (3), A-law (6), µ-law (7), or WAVE_FORMAT_EXTENSIBLE (0xFFFE)
// naming one of them — the sample frames are the data chunk's bytes over the
// fmt chunk's block alignment. A compressed WAV states its sample count only
// in a fact chunk; without one it is refused. A data chunk that claims more
// than the file holds (a stream written with a placeholder size) is measured
// by the bytes present: only those play.
func wavDuration(data []byte) (uint64, uint64, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, 0, errors.New("not a RIFF/WAVE file")
	}
	var (
		format, rate, blockAlign uint32
		dataBytes                uint64
		factSamples              uint64
		haveFormat, haveData     bool
		haveFact                 bool
	)
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := uint64(le32(data[offset+4:]))
		body := offset + 8
		available := uint64(len(data) - body)
		switch id {
		case "fmt ":
			if size < 16 || size > available {
				return 0, 0, errors.New("the fmt chunk is truncated")
			}
			chunk := data[body:]
			format, rate, blockAlign = le16(chunk), le32(chunk[4:]), le16(chunk[12:])
			if format == 0xfffe {
				if size < 40 {
					return 0, 0, errors.New("an extensible fmt chunk names no sub-format")
				}
				format = le16(chunk[24:])
			}
			haveFormat = true
		case "fact":
			if size >= 4 && size <= available {
				factSamples, haveFact = uint64(le32(data[body:])), true
			}
		case "data":
			dataBytes, haveData = min(size, available), true
		}
		if size >= available {
			break
		}
		offset = body + int(size) + int(size&1)
	}
	switch {
	case !haveFormat:
		return 0, 0, errors.New("no fmt chunk")
	case !haveData:
		return 0, 0, errors.New("no data chunk")
	case rate == 0:
		return 0, 0, errors.New("the fmt chunk states no sample rate")
	}
	switch format {
	case 1, 3, 6, 7:
		if blockAlign == 0 {
			return 0, 0, errors.New("the fmt chunk states no block alignment")
		}
		return dataBytes / uint64(blockAlign), uint64(rate), nil
	}
	if haveFact {
		return factSamples, uint64(rate), nil
	}
	return 0, 0, errors.New("a compressed WAV with no fact chunk states no sample count")
}

// flacDuration reads a native FLAC stream's STREAMINFO block: its total
// sample count (36 bits) over its sample rate (20 bits). A stream whose
// STREAMINFO states no total (an encoder that did not know it) is refused.
func flacDuration(data []byte) (uint64, uint64, error) {
	if len(data) < 8+34 || string(data[:4]) != "fLaC" {
		return 0, 0, errors.New("not a FLAC stream")
	}
	if data[4]&0x7f != 0 || int(data[5])<<16|int(data[6])<<8|int(data[7]) < 34 {
		return 0, 0, errors.New("the first metadata block is not STREAMINFO")
	}
	return streamInfo(data[8 : 8+34])
}

// streamInfo reads the sample rate and total samples of a FLAC STREAMINFO
// body, shared by native FLAC and FLAC in Ogg.
func streamInfo(info []byte) (uint64, uint64, error) {
	rate := uint64(info[10])<<12 | uint64(info[11])<<4 | uint64(info[12])>>4
	total := uint64(info[13]&0x0f)<<32 | uint64(be32(info[14:18]))
	if total == 0 {
		return 0, 0, errors.New("STREAMINFO states no total sample count")
	}
	return total, rate, nil
}
