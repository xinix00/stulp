package rtsp

import "fmt"

// H.264 komt in stukken over RTP en moet weer aan elkaar.
//
// Drie vormen, en je hebt ze alle drie nodig:
//   - één NAL in één pakket, het eenvoudige geval;
//   - STAP-A: meerdere kleine NALs in één pakket, want SPS en PPS zijn te klein
//     om een eigen pakket waard te zijn;
//   - FU-A: één NAL over meerdere pakketten, want een beeldframe past niet in
//     één netwerkpakket.
//
// Wie alleen de eerste vorm afhandelt krijgt beeld dat het soms doet -- namelijk
// zolang de camera niets groots te melden heeft.

const (
	nalSTAPA = 24
	nalFUA   = 28
)

// Assembler zet pakketten om in complete toegangseenheden: alle NALs die bij
// hetzelfde moment horen.
type Assembler struct {
	pending   []byte // het FU-A-fragment dat nog niet af is
	unit      [][]byte
	timestamp uint32
	started   bool
}

// Push voegt een pakket toe. Zodra er een compleet frame is komt dat terug.
//
// Het einde van een frame is te zien aan de marker-bit, of aan een tijdstempel
// die verspringt. Dat tweede is de vangnetregel: niet elke camera zet de marker.
func (a *Assembler) Push(packet Packet) (unit [][]byte, timestamp uint32, ok bool) {
	if len(packet.Payload) == 0 {
		return nil, 0, false
	}
	if a.started && packet.Timestamp != a.timestamp && len(a.unit) > 0 {
		unit, timestamp = a.unit, a.timestamp
		a.unit, a.pending = nil, nil
		a.timestamp = packet.Timestamp
		a.append(packet)
		return unit, timestamp, true
	}
	if !a.started {
		a.started = true
		a.timestamp = packet.Timestamp
	}
	// Een nieuw beeld neemt de tijdstempel van zijn eigen pakketten over.
	//
	// Zonder deze regel blijft de tijdstempel staan op die van het vorige beeld
	// zodra dat op de marker is afgesloten: dan draagt elk volgend frame de tijd
	// van zijn voorganger, en een speler ziet één beeld op één moment in plaats
	// van een film. Gevonden tegen een echte camera; de tests hadden het niet,
	// want daar delen alle pakketten één tijdstempel of ontbreekt de marker.
	if len(a.unit) == 0 && len(a.pending) == 0 {
		a.timestamp = packet.Timestamp
	}
	a.append(packet)
	if packet.Marker && len(a.unit) > 0 {
		unit, timestamp = a.unit, a.timestamp
		a.unit, a.pending = nil, nil
		return unit, timestamp, true
	}
	return nil, 0, false
}

func (a *Assembler) append(packet Packet) {
	payload := packet.Payload
	switch kind := payload[0] & 0x1F; kind {
	case nalSTAPA:
		// Lengte, NAL, lengte, NAL, ... Elke lengte is zestien bits.
		rest := payload[1:]
		for len(rest) >= 2 {
			size := int(rest[0])<<8 | int(rest[1])
			rest = rest[2:]
			if size <= 0 || size > len(rest) {
				return
			}
			a.unit = append(a.unit, clone(rest[:size]))
			rest = rest[size:]
		}
	case nalFUA:
		if len(payload) < 2 {
			return
		}
		header := payload[1]
		start := header&0x80 != 0
		end := header&0x40 != 0
		if start {
			// De oorspronkelijke NAL-kop weer opbouwen: de bovenste bits komen
			// van het FU-pakket, het type uit de FU-kop.
			a.pending = []byte{(payload[0] & 0xE0) | (header & 0x1F)}
		}
		if a.pending == nil {
			// Midden in een fragment binnengekomen. De rest van dit frame is
			// toch onbruikbaar, dus wachten op de volgende start.
			return
		}
		a.pending = append(a.pending, payload[2:]...)
		if end {
			a.unit = append(a.unit, a.pending)
			a.pending = nil
		}
	default:
		a.unit = append(a.unit, clone(payload))
	}
}

// IsKeyframe zegt of deze eenheid op zichzelf te bekijken is. Een speler kan
// pas beginnen bij zo'n frame; alles ervoor verwijst naar beelden die hij nooit
// gezien heeft.
func IsKeyframe(unit [][]byte) bool {
	for _, nal := range unit {
		if len(nal) > 0 && nal[0]&0x1F == 5 {
			return true
		}
	}
	return false
}

// SPSInfo is wat er uit de parameterset te halen valt.
type SPSInfo struct {
	Width, Height int
	ProfileIDC    byte
	ConstraintSet byte
	LevelIDC      byte
}

// ParseSPS leest de afmeting uit de parameterset.
//
// Nodig omdat het doosje eromheen de afmeting moet noemen, en die staat nergens
// anders: RTSP zegt het niet en de camera evenmin. Dit leest precies zoveel van
// de bitstroom als daarvoor nodig is en niet meer.
func ParseSPS(sps []byte) (SPSInfo, error) {
	if len(sps) < 4 {
		return SPSInfo{}, fmt.Errorf("h264: parameter set of %d bytes is too short", len(sps))
	}
	info := SPSInfo{ProfileIDC: sps[1], ConstraintSet: sps[2], LevelIDC: sps[3]}
	reader := &bitReader{data: unescape(sps[4:])}

	reader.ue() // seq_parameter_set_id
	switch info.ProfileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := reader.ue()
		if chroma == 3 {
			reader.bit() // separate_colour_plane_flag
		}
		reader.ue() // bit_depth_luma_minus8
		reader.ue() // bit_depth_chroma_minus8
		reader.bit()
		if reader.bit() == 1 { // seq_scaling_matrix_present_flag
			count := 8
			if chroma == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				if reader.bit() == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + reader.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	reader.ue() // log2_max_frame_num_minus4
	if order := reader.ue(); order == 0 {
		reader.ue()
	} else if order == 1 {
		reader.bit()
		reader.se()
		reader.se()
		for count := reader.ue(); count > 0; count-- {
			reader.se()
		}
	}
	reader.ue() // max_num_ref_frames
	reader.bit()
	widthInMBs := reader.ue() + 1
	heightInMapUnits := reader.ue() + 1
	frameMBsOnly := reader.bit()
	if frameMBsOnly == 0 {
		reader.bit() // mb_adaptive_frame_field_flag
	}
	reader.bit() // direct_8x8_inference_flag

	width := widthInMBs * 16
	height := heightInMapUnits * 16
	if frameMBsOnly == 0 {
		height *= 2
	}
	if reader.bit() == 1 { // frame_cropping_flag
		left, right, top, bottom := reader.ue(), reader.ue(), reader.ue(), reader.ue()
		// De eenheid van het bijsnijden hangt van het kleurformaat af; 4:2:0 is
		// wat elke camera stuurt, en dan is het twee pixels breed en twee hoog
		// (vier bij een interlaced beeld).
		width -= (left + right) * 2
		height -= (top + bottom) * 2
		if frameMBsOnly == 0 {
			height -= (top + bottom) * 2
		}
	}
	if reader.failed || width <= 0 || height <= 0 {
		return SPSInfo{}, fmt.Errorf("h264: could not read the picture size from the parameter set")
	}
	info.Width, info.Height = width, height
	return info, nil
}

// unescape haalt de emulation-prevention-bytes eruit: een 0x03 die tussen twee
// nullen is gezet zodat de bitstroom niet op een startcode lijkt.
func unescape(data []byte) []byte {
	out := make([]byte, 0, len(data))
	zeros := 0
	for _, b := range data {
		if zeros == 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader leest de exp-Golomb-codes waar H.264 uit bestaat.
type bitReader struct {
	data   []byte
	offset int
	failed bool
}

func (r *bitReader) bit() int {
	if r.offset >= len(r.data)*8 {
		r.failed = true
		return 0
	}
	value := int(r.data[r.offset/8]>>(7-r.offset%8)) & 1
	r.offset++
	return value
}

func (r *bitReader) ue() int {
	zeros := 0
	for r.bit() == 0 && !r.failed {
		zeros++
		if zeros > 32 {
			r.failed = true
			return 0
		}
	}
	value := 1
	for i := 0; i < zeros; i++ {
		value = value<<1 | r.bit()
	}
	return value - 1
}

func (r *bitReader) se() int {
	value := r.ue()
	if value%2 == 0 {
		return -value / 2
	}
	return (value + 1) / 2
}

func clone(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	return out
}
