package rtsp

import (
	"encoding/binary"
	"fmt"
)

// AV1 over RTP, en AV1 in een MP4.
//
// Anders dan H.264 op twee punten die er allebei toe doen. Er staan geen
// parametersets in de beschrijving: de sequence header komt in de stroom mee, en
// het doosje kan dus pas geschreven worden als het eerste beeld binnen is. En de
// stukken heten OBU's in plaats van NAL's, met hun lengte in LEB128 in plaats
// van vier vaste bytes.
//
// Wat gelijk blijft is de rest van de MP4: dezelfde moov, dezelfde moof, alleen
// een av01 in plaats van een avc1. Een browser speelt het zonder hulp -- Chrome,
// Edge en Firefox doen AV1, en Safari op Apple silicon ook.

// OBU-types, voor zover deze code ze onderscheidt.
const (
	obuSequenceHeader = 1
	obuTemporalDelim  = 2
	obuFrame          = 6
	obuFrameHeader    = 3
)

// AV1Assembler zet RTP-pakketten om in complete beeldeenheden.
//
// De aggregatiekop is één byte: Z zegt dat het eerste stuk het vervolg is van
// het vorige pakket, Y dat het laatste stuk verdergaat in het volgende, W hoeveel
// stukken erin zitten (nul betekent: lees tot het pakket op is), en N dat hier
// een nieuwe reeks begint.
type AV1Assembler struct {
	pending   []byte
	unit      [][]byte
	timestamp uint32
	started   bool
	// sequence bewaart de laatste sequence header. Het doosje heeft hem nodig,
	// en een kijker die later inschakelt ook.
	sequence []byte
}

// Push voegt een pakket toe en levert een complete eenheid zodra die er is.
func (a *AV1Assembler) Push(packet Packet) (unit [][]byte, timestamp uint32, ok bool) {
	if len(packet.Payload) < 1 {
		return nil, 0, false
	}
	if a.started && packet.Timestamp != a.timestamp && len(a.unit) > 0 {
		unit, timestamp = a.unit, a.timestamp
		a.unit = nil
		a.timestamp = packet.Timestamp
		a.append(packet.Payload)
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
	a.append(packet.Payload)
	if packet.Marker && len(a.unit) > 0 {
		unit, timestamp = a.unit, a.timestamp
		a.unit = nil
		return unit, timestamp, true
	}
	return nil, 0, false
}

func (a *AV1Assembler) append(payload []byte) {
	header := payload[0]
	continues := header&0x80 != 0 // Z
	willContinue := header&0x40 != 0
	count := int(header>>4) & 0x03 // W
	rest := payload[1:]

	for index := 0; len(rest) > 0; index++ {
		var element []byte
		if count > 0 && index == count-1 {
			// Het laatste stuk draagt geen lengte: het loopt tot het einde.
			element, rest = rest, nil
		} else {
			size, read := readLEB128(rest)
			if read == 0 || size > uint64(len(rest)-read) {
				return // een lengte die niet past: de rest is onbruikbaar
			}
			element = rest[read : read+int(size)]
			rest = rest[read+int(size):]
		}
		switch {
		case index == 0 && continues:
			// Vervolg van het vorige pakket.
			if a.pending == nil {
				continue // begin gemist; wachten op een heel stuk
			}
			a.pending = append(a.pending, element...)
		default:
			a.flushPending()
			a.pending = clone(element)
		}
		if len(rest) == 0 && willContinue {
			return // loopt door in het volgende pakket
		}
		if len(rest) == 0 {
			a.flushPending()
		}
	}
}

func (a *AV1Assembler) flushPending() {
	if len(a.pending) == 0 {
		return
	}
	obu := a.pending
	a.pending = nil
	if kind := (obu[0] >> 3) & 0x0F; kind == obuTemporalDelim {
		// De scheiding tussen twee beelden draagt niets en hoort niet in de
		// MP4: die telt zijn samples zelf.
		return
	} else if kind == obuSequenceHeader {
		a.sequence = clone(obu)
	}
	a.unit = append(a.unit, obu)
}

// SequenceHeader is de beschrijving van het beeld, zodra hij langsgekomen is.
func (a *AV1Assembler) SequenceHeader() []byte { return a.sequence }

// IsAV1Keyframe zegt of deze eenheid op zichzelf te bekijken is.
//
// Dit stond er eerst als "draagt deze eenheid een sequence header", op de
// aanname dat AV1 die alleen bij een keyframe herhaalt. Een echte UniFi-camera
// weerlegt dat: die stuurt de sequence header bij élk beeld mee. Daarmee gold
// het eerste beeld altijd als keyframe, begon de stream op een INTER-beeld en
// zag een speler nooit iets -- er was geen referentie om op te bouwen.
//
// Wat wél telt staat in de beeldkop zelf: het eerste bit zegt of dit een eerder
// gedecodeerd beeld hertoont, en de twee bits erna zeggen welk soort beeld het
// is. Alleen KEY_FRAME is een plek om in te stappen.
func IsAV1Keyframe(unit [][]byte) bool {
	for _, obu := range unit {
		if len(obu) == 0 {
			continue
		}
		switch (obu[0] >> 3) & 0x0F {
		case obuFrame, obuFrameHeader:
		default:
			continue
		}
		payload, err := obuPayload(obu)
		if err != nil || len(payload) == 0 {
			continue
		}
		// show_existing_frame: dit beeld is er al, er wordt niets nieuws
		// gedecodeerd. Nooit een startpunt.
		if payload[0]&0x80 != 0 {
			return false
		}
		return (payload[0]>>5)&0x03 == frameTypeKey
	}
	return false
}

// frameTypeKey is de waarde van frame_type voor een keyframe. De andere drie
// (inter, intra-only, switch) leunen allemaal op beelden die er al zijn.
const frameTypeKey = 0

// readLEB128 leest een lengte met veranderlijke breedte.
func readLEB128(data []byte) (value uint64, read int) {
	for index := 0; index < len(data) && index < 8; index++ {
		value |= uint64(data[index]&0x7F) << (index * 7)
		if data[index]&0x80 == 0 {
			return value, index + 1
		}
	}
	return 0, 0
}

// writeLEB128 schrijft er een.
func writeLEB128(value uint64) []byte {
	out := make([]byte, 0, 8)
	for {
		part := byte(value & 0x7F)
		value >>= 7
		if value != 0 {
			part |= 0x80
		}
		out = append(out, part)
		if value == 0 {
			return out
		}
	}
}

// AV1Info is wat er uit de sequence header te halen valt.
type AV1Info struct {
	Profile, Level, Tier byte
	Width, Height        int
	HighBitdepth         bool
}

// ParseAV1SequenceHeader leest profiel, niveau en afmeting.
//
// De afmeting staat achteraan de kop, dus alles ervoor moet precies overgeslagen
// worden -- een bit te veel of te weinig en je leest onzin als beeldbreedte.
// Vandaar dat de optionele blokken hier echt doorlopen worden en niet geraden:
// timing, het decodermodel en de vertraging per operating point zitten er bij
// een echte camera gewoon in.
func ParseAV1SequenceHeader(obu []byte) (AV1Info, error) {
	payload, err := obuPayload(obu)
	if err != nil {
		return AV1Info{}, err
	}
	reader := &bitReader{data: payload}
	bufferDelay := 0
	info := AV1Info{}
	info.Profile = byte(readBits(reader, 3))
	readBits(reader, 1) // still_picture
	reduced := readBits(reader, 1) == 1

	if reduced {
		info.Level = byte(readBits(reader, 5))
	} else {
		decoderModel := false
		if readBits(reader, 1) == 1 { // timing_info_present_flag
			readBits(reader, 32) // num_units_in_display_tick
			readBits(reader, 32) // time_scale
			if readBits(reader, 1) == 1 {
				readUVLC(reader) // num_ticks_per_picture_minus_1
			}
			if readBits(reader, 1) == 1 { // decoder_model_info_present_flag
				decoderModel = true
				bufferDelay = int(readBits(reader, 5)) + 1
				readBits(reader, 32) // num_units_in_decoding_tick
				readBits(reader, 5)  // buffer_removal_time_length_minus_1
				readBits(reader, 5)  // frame_presentation_time_length_minus_1
			}
		}
		initialDelay := readBits(reader, 1) == 1
		points := int(readBits(reader, 5)) + 1
		for point := 0; point < points; point++ {
			readBits(reader, 12) // operating_point_idc
			level := byte(readBits(reader, 5))
			var tier byte
			if level > 7 {
				tier = byte(readBits(reader, 1))
			}
			if point == 0 {
				info.Level, info.Tier = level, tier
			}
			if decoderModel && readBits(reader, 1) == 1 {
				// operating_parameters_info: twee vertragingen plus een vlag.
				readBits(reader, bufferDelay)
				readBits(reader, bufferDelay)
				readBits(reader, 1)
			}
			if initialDelay && readBits(reader, 1) == 1 {
				readBits(reader, 4)
			}
		}
	}
	widthBits := int(readBits(reader, 4)) + 1
	heightBits := int(readBits(reader, 4)) + 1
	info.Width = int(readBits(reader, widthBits)) + 1
	info.Height = int(readBits(reader, heightBits)) + 1
	if reader.failed || info.Width <= 0 || info.Height <= 0 {
		return AV1Info{}, fmt.Errorf("av1: could not read the picture size from the sequence header")
	}
	return info, nil
}

// readUVLC leest een getal met veranderlijke lengte, zoals AV1 dat kent.
func readUVLC(reader *bitReader) uint64 {
	zeros := 0
	for zeros < 32 && reader.bit() == 0 && !reader.failed {
		zeros++
	}
	if zeros >= 32 {
		return 0
	}
	return readBits(reader, zeros) + (1 << zeros) - 1
}

// obuPayload haalt de kop van een OBU eraf.
func obuPayload(obu []byte) ([]byte, error) {
	if len(obu) < 1 {
		return nil, fmt.Errorf("av1: empty OBU")
	}
	offset := 1
	if obu[0]&0x04 != 0 { // obu_extension_flag
		offset++
	}
	if obu[0]&0x02 != 0 { // obu_has_size_field
		_, read := readLEB128(obu[offset:])
		if read == 0 {
			return nil, fmt.Errorf("av1: OBU announces an unreadable size")
		}
		offset += read
	}
	if offset >= len(obu) {
		return nil, fmt.Errorf("av1: OBU has no payload")
	}
	return obu[offset:], nil
}

// readBits leest n bits als getal.
func readBits(reader *bitReader, count int) uint64 {
	var value uint64
	for index := 0; index < count; index++ {
		value = value<<1 | uint64(reader.bit())
	}
	return value
}

// av1C draagt de beschrijving van het beeld, zoals avcC dat bij H.264 doet.
//
// De sequence header gaat er ongewijzigd in: een speler leest hem daar en heeft
// hem dan al voordat het eerste beeld komt.
func av1C(info AV1Info, sequence []byte) []byte {
	body := make([]byte, 0, 4+len(sequence))
	body = append(body, 0x81)                            // marker 1, versie 1
	body = append(body, info.Profile<<5|info.Level&0x1F) //
	bits := info.Tier << 7                               //
	if info.HighBitdepth {
		bits |= 1 << 6
	}
	body = append(body, bits)
	body = append(body, 0) // geen initial presentation delay
	body = append(body, sequence...)
	return box("av1C", body)
}

// av01 is de sample entry: hetzelfde als avc1, met av1C erin.
func av01(info AV1Info, sequence []byte) []byte {
	body := make([]byte, 0, 86+len(sequence))
	body = append(body, make([]byte, 6)...)
	body = append(body, u16(1)...)
	body = append(body, make([]byte, 16)...)
	body = append(body, u16(uint16(info.Width))...)
	body = append(body, u16(uint16(info.Height))...)
	body = append(body, u32(0x00480000)...)
	body = append(body, u32(0x00480000)...)
	body = append(body, u32(0)...)
	body = append(body, u16(1)...)
	body = append(body, make([]byte, 32)...)
	body = append(body, u16(0x0018)...)
	body = append(body, 0xFF, 0xFF)
	body = append(body, av1C(info, sequence)...)
	return box("av01", body)
}

// AV1MimeType is wat een browser moet weten om de MediaSource op te zetten.
func AV1MimeType(info AV1Info) string {
	return fmt.Sprintf("video/mp4; codecs=\"av01.%d.%02d%s.08\"",
		info.Profile, info.Level, map[byte]string{0: "M", 1: "H"}[info.Tier])
}

// av1Sample zet de OBU's van één beeld achter elkaar, met hun lengte erin.
//
// In een MP4 hoort elke OBU zijn eigen lengteveld te dragen; over RTP zit die
// lengte in het pakket en niet in de OBU zelf. Vandaar dat de vlag gezet wordt
// en de lengte erbij geschreven.
func av1Sample(unit [][]byte) []byte {
	sample := make([]byte, 0, 4096)
	for _, obu := range unit {
		if len(obu) == 0 {
			continue
		}
		if obu[0]&0x02 != 0 {
			// Draagt zijn lengte al.
			sample = append(sample, obu...)
			continue
		}
		header := obu[0] | 0x02
		rest := obu[1:]
		if obu[0]&0x04 != 0 && len(obu) > 1 {
			// De uitbreidingsbyte hoort vóór het lengteveld te blijven staan.
			sample = append(sample, header, obu[1])
			rest = obu[2:]
			sample = append(sample, writeLEB128(uint64(len(rest)))...)
			sample = append(sample, rest...)
			continue
		}
		sample = append(sample, header)
		sample = append(sample, writeLEB128(uint64(len(rest)))...)
		sample = append(sample, rest...)
	}
	return sample
}

var _ = binary.BigEndian
