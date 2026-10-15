package audioprobe

import (
	"errors"
	"fmt"
	"math/bits"
)

// mp4Track is one track's identity and timing.
type mp4Track struct {
	id        uint32
	sound     bool
	timescale uint32
	// ticks is the sum of the sample table's durations (stts).
	ticks uint64
	// defaultDuration is the movie extends' default sample duration (trex).
	defaultDuration uint32
}

// mp4Duration is the sound track's samples over its timescale: the sample
// table (stts) of the track itself and, in a fragmented file — the kind
// Safari's MediaRecorder writes, whose header durations are zero — every
// fragment's track run (trun) for that track, with the fragment's or the
// movie's default sample duration where a run states none. The header
// durations (mvhd, mdhd, tkhd) are not read: the sample tables are what
// plays.
func mp4Duration(data []byte) (uint64, uint64, error) {
	var (
		tracks    []*mp4Track
		fragments [][]byte
		defaults  = map[uint32]uint32{}
		sawMovie  bool
	)
	if err := eachBox(data, func(kind string, body []byte) error {
		switch kind {
		case "moov":
			sawMovie = true
			return eachBox(body, func(kind string, body []byte) error {
				switch kind {
				case "trak":
					track, err := mp4ReadTrack(body)
					if err != nil {
						return err
					}
					tracks = append(tracks, track)
				case "mvex":
					return eachBox(body, func(kind string, body []byte) error {
						if kind == "trex" && len(body) >= 16 {
							// track_ID, default sample description index,
							// default sample duration.
							defaults[be32(body[4:])] = be32(body[12:])
						}
						return nil
					})
				}
				return nil
			})
		case "moof":
			fragments = append(fragments, body)
		}
		return nil
	}); err != nil {
		return 0, 0, err
	}
	if !sawMovie {
		return 0, 0, errors.New("no movie box")
	}
	var sound *mp4Track
	for _, track := range tracks {
		if !track.sound {
			continue
		}
		if sound != nil {
			return 0, 0, errors.New("more than one sound track")
		}
		sound = track
	}
	if sound == nil {
		return 0, 0, errors.New("no sound track")
	}
	sound.defaultDuration = defaults[sound.id]
	ticks := sound.ticks
	for _, fragment := range fragments {
		fragmentTicks, err := mp4FragmentTicks(fragment, sound)
		if err != nil {
			return 0, 0, err
		}
		var carry uint64
		if ticks, carry = bits.Add64(ticks, fragmentTicks, 0); carry != 0 {
			return 0, 0, errors.New("a duration past what a duration can hold")
		}
	}
	if ticks == 0 {
		return 0, 0, errors.New("the sound track has no samples")
	}
	return ticks, uint64(sound.timescale), nil
}

func mp4ReadTrack(trak []byte) (*mp4Track, error) {
	track := &mp4Track{}
	err := eachBox(trak, func(kind string, body []byte) error {
		switch kind {
		case "tkhd":
			at := 12 // version 0: version/flags, creation and modification times
			if len(body) > 0 && body[0] == 1 {
				at = 20
			}
			if len(body) < at+4 {
				return errors.New("a truncated track header")
			}
			track.id = be32(body[at:])
		case "mdia":
			return eachBox(body, func(kind string, body []byte) error {
				switch kind {
				case "hdlr":
					track.sound = len(body) >= 12 && string(body[8:12]) == "soun"
				case "mdhd":
					at := 12
					if len(body) > 0 && body[0] == 1 {
						at = 20
					}
					if len(body) < at+4 {
						return errors.New("a truncated media header")
					}
					track.timescale = be32(body[at:])
				case "minf":
					return eachBox(body, func(kind string, body []byte) error {
						if kind != "stbl" {
							return nil
						}
						return eachBox(body, func(kind string, body []byte) error {
							if kind != "stts" {
								return nil
							}
							ticks, err := mp4SampleTable(body)
							track.ticks = ticks
							return err
						})
					})
				}
				return nil
			})
		}
		return nil
	})
	if err == nil && track.sound && track.timescale == 0 {
		err = errors.New("the sound track states no timescale")
	}
	return track, err
}

// mp4SampleTable sums a time-to-sample table: count × delta per entry.
func mp4SampleTable(stts []byte) (uint64, error) {
	if len(stts) < 8 {
		return 0, errors.New("a truncated time-to-sample table")
	}
	entries := int(be32(stts[4:]))
	if len(stts) < 8+8*entries {
		return 0, errors.New("a truncated time-to-sample table")
	}
	var total uint64
	for index := range entries {
		entry := stts[8+8*index:]
		high, product := bits.Mul64(uint64(be32(entry)), uint64(be32(entry[4:])))
		var carry uint64
		total, carry = bits.Add64(total, product, 0)
		if high != 0 || carry != 0 {
			return 0, errors.New("a duration past what a duration can hold")
		}
	}
	return total, nil
}

// mp4FragmentTicks sums one movie fragment's runs for the sound track.
func mp4FragmentTicks(moof []byte, sound *mp4Track) (uint64, error) {
	var total uint64
	err := eachBox(moof, func(kind string, traf []byte) error {
		if kind != "traf" {
			return nil
		}
		var (
			mine     bool
			fallback = sound.defaultDuration
		)
		return eachBox(traf, func(kind string, body []byte) error {
			switch kind {
			case "tfhd":
				if len(body) < 8 {
					return errors.New("a truncated fragment header")
				}
				flags := be32(body) & 0xffffff
				mine = be32(body[4:]) == sound.id
				at := 8
				if flags&0x01 != 0 {
					at += 8 // base data offset
				}
				if flags&0x02 != 0 {
					at += 4 // sample description index
				}
				if flags&0x08 != 0 {
					if len(body) < at+4 {
						return errors.New("a truncated fragment header")
					}
					fallback = be32(body[at:])
				}
			case "trun":
				if !mine {
					return nil
				}
				ticks, err := mp4Run(body, fallback)
				total += ticks
				return err
			}
			return nil
		})
	})
	return total, err
}

// mp4Run sums one track run's sample durations (ISO/IEC 14496-12 §8.8.8).
func mp4Run(trun []byte, fallback uint32) (uint64, error) {
	if len(trun) < 8 {
		return 0, errors.New("a truncated track run")
	}
	flags := be32(trun) & 0xffffff
	count := int(be32(trun[4:]))
	at := 8
	if flags&0x01 != 0 {
		at += 4 // data offset
	}
	if flags&0x04 != 0 {
		at += 4 // first sample flags
	}
	if flags&0x100 == 0 {
		if fallback == 0 {
			return 0, errors.New("a track run states no sample duration and no default")
		}
		return uint64(count) * uint64(fallback), nil
	}
	stride := 4
	for _, present := range []uint32{0x200, 0x400, 0x800} {
		if flags&present != 0 {
			stride += 4
		}
	}
	if len(trun) < at+stride*count {
		return 0, errors.New("a truncated track run")
	}
	var total uint64
	for index := range count {
		total += uint64(be32(trun[at+stride*index:]))
	}
	return total, nil
}

// eachBox visits the boxes of a body: a 32-bit size (1: a 64-bit size
// follows the type; 0: to the end) and a four-character type.
func eachBox(data []byte, visit func(kind string, body []byte) error) error {
	for offset := 0; offset < len(data); {
		if len(data)-offset < 8 {
			return fmt.Errorf("a truncated box at byte %d", offset)
		}
		size := uint64(be32(data[offset:]))
		kind := string(data[offset+4 : offset+8])
		header := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data) - offset)
		case 1:
			if len(data)-offset < 16 {
				return fmt.Errorf("a truncated box at byte %d", offset)
			}
			size, header = be64(data[offset+8:]), 16
		}
		if size < header || size > uint64(len(data)-offset) {
			return fmt.Errorf("the %q box at byte %d overruns the file", kind, offset)
		}
		if err := visit(kind, data[uint64(offset)+header:uint64(offset)+size]); err != nil {
			return err
		}
		offset += int(size)
	}
	return nil
}
