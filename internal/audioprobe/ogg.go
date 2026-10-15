package audioprobe

import (
	"errors"
	"fmt"
)

// oggDuration reads an Ogg file's one logical audio stream: the granule
// position of its last page that carries one, which the codec mapping defines
// as the sample count at that point — Opus at 48 kHz less the pre-skip its
// header declares (RFC 7845), Vorbis and FLAC at their own sample rate. A
// multiplexed or chained file, or a codec other than these three, is refused.
func oggDuration(data []byte) (uint64, uint64, error) {
	var (
		serial         uint32
		found          bool
		rate, preSkip  uint64
		lastGranule    uint64
		granuleCarried bool
	)
	for offset := 0; offset < len(data); {
		page := data[offset:]
		if len(page) < 27 || string(page[:4]) != "OggS" || page[4] != 0 {
			return 0, 0, fmt.Errorf("no Ogg page at byte %d", offset)
		}
		segments := int(page[26])
		if len(page) < 27+segments {
			return 0, 0, fmt.Errorf("the page at byte %d is truncated", offset)
		}
		bodyLength := 0
		for _, lace := range page[27 : 27+segments] {
			bodyLength += int(lace)
		}
		end := 27 + segments + bodyLength
		if end > len(page) {
			return 0, 0, fmt.Errorf("the page at byte %d is truncated", offset)
		}
		body := page[27+segments : end]
		pageSerial := le32(page[14:])
		if page[5]&0x02 != 0 { // beginning of a logical stream
			if found {
				return 0, 0, errors.New("more than one logical stream")
			}
			var err error
			if rate, preSkip, err = oggCodec(body); err != nil {
				return 0, 0, err
			}
			serial, found = pageSerial, true
		} else if !found {
			return 0, 0, errors.New("the first page does not begin a stream")
		}
		if pageSerial != serial {
			return 0, 0, errors.New("more than one logical stream")
		}
		// -1 marks a page on which no packet ends.
		if granule := le64(page[6:]); granule != ^uint64(0) {
			lastGranule, granuleCarried = granule, true
		}
		offset += end
	}
	if !found || !granuleCarried {
		return 0, 0, errors.New("no page states a granule position")
	}
	if lastGranule <= preSkip {
		return 0, 0, errors.New("no audio past the pre-skip")
	}
	return lastGranule - preSkip, rate, nil
}

// oggCodec identifies the stream from its first packet and returns the
// granule rate and the samples to skip.
func oggCodec(packet []byte) (rate, preSkip uint64, err error) {
	switch {
	case len(packet) >= 19 && string(packet[:8]) == "OpusHead":
		return 48_000, uint64(le16(packet[10:])), nil
	case len(packet) >= 30 && string(packet[:7]) == "\x01vorbis":
		return uint64(le32(packet[12:])), 0, nil
	case len(packet) >= 51 && string(packet[:5]) == "\x7fFLAC" && string(packet[9:13]) == "fLaC":
		flacRate, flacErr := streamInfoRate(packet[17:51])
		return flacRate, 0, flacErr
	}
	return 0, 0, errors.New("the Ogg stream is not Opus, Vorbis or FLAC")
}

// streamInfoRate reads only the sample rate of a FLAC STREAMINFO body: in Ogg
// the total is the granule position, not STREAMINFO's.
func streamInfoRate(info []byte) (uint64, error) {
	rate := uint64(info[10])<<12 | uint64(info[11])<<4 | uint64(info[12])>>4
	if rate == 0 {
		return 0, errors.New("STREAMINFO states no sample rate")
	}
	return rate, nil
}
