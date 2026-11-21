package music

// ADTS AAC, as the audio of a segment is kept: frames of 1024 samples, each
// with a header of its length and sample rate.

var adtsRates = [...]float64{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// adtsFrame is the length and the seconds of the ADTS frame at the start of
// data; false if there is none.
func adtsFrame(data []byte) (int, float64, bool) {
	if len(data) < 7 || data[0] != 0xff || data[1]&0xf6 != 0xf0 {
		return 0, 0, false
	}
	rate := int(data[2]>>2) & 0x0f
	length := int(data[3]&0x03)<<11 | int(data[4])<<3 | int(data[5])>>5
	if rate >= len(adtsRates) || length < 7 || length > len(data) {
		return 0, 0, false
	}
	return length, float64(1024*(int(data[6]&0x03)+1)) / adtsRates[rate], true
}

// adtsCut is the audio of data from skip seconds into it, at most keep
// seconds of it, by whole frames, and the seconds it starts at and lasts.
// Data that isn't ADTS is kept whole: 0 and -1.
func adtsCut(data []byte, skip, keep float64) ([]byte, float64, float64) {
	var at, from, to float64
	start, end := -1, 0
	for i := 0; i < len(data); {
		n, seconds, ok := adtsFrame(data[i:])
		if !ok {
			if start < 0 && i == 0 {
				return data, 0, -1
			}
			break
		}
		if at+seconds/2 >= skip && start < 0 {
			start, from = i, at
		}
		if start >= 0 {
			if at+seconds/2-from > keep {
				break
			}
			end, to = i+n, at+seconds
		}
		at += seconds
		i += n
	}
	if start < 0 {
		return nil, at, 0
	}
	return data[start:end], from, to - from
}
