package rtsp

import (
	"encoding/binary"
	"fmt"
)

// Frames in een doosje dat een browser opent.
//
// fMP4 en niet iets slimmers: een browser speelt het met een gewone <video> en
// een MediaSource, er is geen bibliotheek voor nodig, en het is te schrijven
// terwijl het binnenkomt. Een gewone MP4 kan dat niet -- die wil vooraf weten
// hoe lang de film is, en een camera weet dat nooit.
//
// De opbouw: eerst één keer een kopdeel (ftyp + moov) dat zegt wat voor beeld
// het is, daarna per frame een moof met een mdat erachter. Wie later inschakelt
// krijgt het kopdeel opnieuw en kan meteen mee.
//
// Eén aanname: geen herordening. Elk beeld wordt getoond op het moment waarop het
// gedecodeerd wordt, want er worden geen composition-time offsets geschreven. Een
// livestream van een camera heeft die ook niet -- het gaat om zo weinig
// vertraging mogelijk, dus stuurt hij I- en P-beelden en geen B-beelden. Komt er
// ooit een camera die wel herordent, dan is dit de plek: het beeld zou anders in
// de verkeerde volgorde langskomen.

// timescale is de eenheid waarin tijden geteld worden. 90 kHz is wat H.264 over
// RTP gebruikt, dus de tijdstempels kunnen ongewijzigd mee.
const timescale = 90000

// Muxer maakt fMP4-stukken.
//
// Hij schrijft niet zelf maar geeft terug: het kopdeel één keer, daarna een
// fragment per frame. Zo kan hetzelfde fragment naar meerdere kijkers tegelijk,
// en krijgt wie later inschakelt het kopdeel zonder dat er iets opnieuw
// berekend hoeft te worden.
type Muxer struct {
	codec    Codec
	header   []byte
	mime     string
	sequence uint32
	last     uint32
	haveLast bool
	// first is de RTP-tijdstempel van het eerste beeld, en wraps telt hoe vaak
	// de 32-bits teller sindsdien omgelopen is. Samen maken ze van een
	// willekeurig beginpunt een tijdlijn die op nul begint. Zie Fragment.
	first uint32
	wraps uint64
}

// NewMuxer stelt het kopdeel samen uit de H.264-parametersets.
func NewMuxer(sps, pps []byte) (*Muxer, error) {
	info, err := ParseSPS(sps)
	if err != nil {
		return nil, err
	}
	return &Muxer{
		codec:  H264,
		header: append(ftyp(), moov(sampleEntry(avc1(info, sps, pps)), info.Width, info.Height)...),
		mime:   MimeType(info),
	}, nil
}

// NewAV1Muxer doet hetzelfde voor AV1.
//
// De sequence header komt uit de stroom en niet uit de beschrijving, dus deze
// muxer kan pas gemaakt worden als het eerste beeld binnen is. Dat is meteen de
// reden dat hij apart staat: bij H.264 weet je alles vooraf.
func NewAV1Muxer(sequence []byte) (*Muxer, error) {
	info, err := ParseAV1SequenceHeader(sequence)
	if err != nil {
		return nil, err
	}
	return &Muxer{
		codec:  AV1,
		header: append(ftyp(), moov(sampleEntry(av01(info, sequence)), info.Width, info.Height)...),
		mime:   AV1MimeType(info),
	}, nil
}

// MimeType is wat de browser moet weten om de MediaSource op te zetten.
func (m *Muxer) MimeType() string { return m.mime }

// Header is wat een speler als eerste moet krijgen: wat voor beeld dit is.
func (m *Muxer) Header() []byte { return m.header }

// Fragment maakt één toegangseenheid klaar om te versturen.
//
// De duur volgt uit het verschil met de vorige tijdstempel. Bij het eerste frame
// is er niets om mee te vergelijken; dan wordt 1/25 seconde aangenomen, en dat
// is één frame lang zichtbaar voordat de echte tijden het overnemen.
//
// De tijdlijn begint op nul, en dat is geen kosmetiek. RTP schrijft voor dat de
// teller op een willekeurige waarde begint (RFC 3550), dus een camera levert een
// eerste beeld dat net zo goed op dertien uur kan liggen. Een speler begint bij
// nul en wacht dan eeuwig op beeld dat daar had moeten staan -- zonder fout, want
// er is niets mis: er is alleen niets. Vandaar het aftrekken van het eerste
// tijdstempel, met een teller voor het omlopen van de 32 bits erbij zodat de
// tijdlijn na dertien uur kijken niet terugspringt.
func (m *Muxer) Fragment(unit [][]byte, timestamp uint32) ([]byte, error) {
	var sample []byte
	if m.codec == AV1 {
		sample = av1Sample(unit)
	} else {
		sample = make([]byte, 0, 4096)
		for _, nal := range unit {
			if len(nal) == 0 {
				continue
			}
			// Lengte-voorvoegsel in plaats van startcodes: dat is wat in een
			// MP4 hoort, en wat avcC aankondigt.
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(len(nal)))
			sample = append(sample, size[:]...)
			sample = append(sample, nal...)
		}
	}
	if len(sample) == 0 {
		return nil, nil
	}
	duration := uint32(timescale / 25)
	if m.haveLast {
		// Het verschil kan overlopen: de teller is 32 bits en gaat om de dertien
		// uur rond. Bij een sprong terug is de vorige duur de beste gok.
		if delta := timestamp - m.last; delta > 0 && delta < timescale*10 {
			duration = delta
		}
		// Ver terug in de tijd is geen hapering maar de teller die omliep. Tien
		// seconden marge, zodat een paar pakketten door elkaar dat niet is.
		if timestamp < m.last && uint64(m.last)-uint64(timestamp) > uint64(timescale)*10 {
			m.wraps++
		}
	} else {
		m.first = timestamp
	}
	m.last, m.haveLast = timestamp, true
	m.sequence++
	decodeTime := m.wraps<<32 + uint64(timestamp) - uint64(m.first)

	fragment := moof(m.sequence, decodeTime, duration, uint32(len(sample)), m.Keyframe(unit))
	return append(fragment, box("mdat", sample)...), nil
}

// Keyframe zegt of een speler hier kan beginnen.
func (m *Muxer) Keyframe(unit [][]byte) bool {
	if m.codec == AV1 {
		return IsAV1Keyframe(unit)
	}
	return IsKeyframe(unit)
}

// box bouwt één MP4-doos: lengte, naam, inhoud.
func box(name string, payload ...[]byte) []byte {
	size := 8
	for _, part := range payload {
		size += len(part)
	}
	out := make([]byte, 0, size)
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(size))
	out = append(out, header[:]...)
	out = append(out, name...)
	for _, part := range payload {
		out = append(out, part...)
	}
	return out
}

func u32(value uint32) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], value)
	return out[:]
}

func u16(value uint16) []byte {
	var out [2]byte
	binary.BigEndian.PutUint16(out[:], value)
	return out[:]
}

func ftyp() []byte {
	return box("ftyp", []byte("isom"), u32(512), []byte("isomiso2avc1mp41"))
}

// sampleEntry verpakt de codec-specifieke doos zodat de rest gelijk blijft.
func sampleEntry(entry []byte) []byte { return entry }

func moov(entry []byte, width, height int) []byte {
	return box("moov", mvhd(), trak(entry, width, height), mvex())
}

func mvhd() []byte {
	body := make([]byte, 0, 108)
	body = append(body, 0, 0, 0, 0)          // versie en vlaggen
	body = append(body, u32(0)...)           // creatie
	body = append(body, u32(0)...)           // wijziging
	body = append(body, u32(timescale)...)   //
	body = append(body, u32(0)...)           // duur nul: het is niet af
	body = append(body, u32(0x00010000)...)  // snelheid 1.0
	body = append(body, u16(0x0100)...)      // volume 1.0
	body = append(body, make([]byte, 10)...) // gereserveerd
	body = append(body, unityMatrix()...)
	body = append(body, make([]byte, 24)...) // vooraf gedefinieerd
	body = append(body, u32(2)...)           // volgende spoor-id
	return box("mvhd", body)
}

// unityMatrix is de eenheidsmatrix waar elk MP4-bestand mee begint: geen
// draaiing, geen schaling.
func unityMatrix() []byte {
	values := []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	out := make([]byte, 0, 36)
	for _, value := range values {
		out = append(out, u32(value)...)
	}
	return out
}

func trak(entry []byte, width, height int) []byte {
	return box("trak", tkhd(width, height), mdia(entry, width, height))
}

func tkhd(width, height int) []byte {
	body := make([]byte, 0, 84)
	body = append(body, 0, 0, 0, 3) // vlaggen: in gebruik en zichtbaar
	body = append(body, u32(0)...)
	body = append(body, u32(0)...)
	body = append(body, u32(1)...) // spoor-id
	body = append(body, u32(0)...)
	body = append(body, u32(0)...) // duur
	body = append(body, make([]byte, 8)...)
	body = append(body, u16(0)...) // laag
	body = append(body, u16(0)...) // alternatiefgroep
	body = append(body, u16(0)...) // volume: beeld heeft er geen
	body = append(body, make([]byte, 2)...)
	body = append(body, unityMatrix()...)
	body = append(body, u32(uint32(width)<<16)...)
	body = append(body, u32(uint32(height)<<16)...)
	return box("tkhd", body)
}

func mdia(entry []byte, width, height int) []byte {
	mdhd := make([]byte, 0, 24)
	mdhd = append(mdhd, 0, 0, 0, 0)
	mdhd = append(mdhd, u32(0)...)
	mdhd = append(mdhd, u32(0)...)
	mdhd = append(mdhd, u32(timescale)...)
	mdhd = append(mdhd, u32(0)...)
	mdhd = append(mdhd, 0x55, 0xC4) // taal: und
	mdhd = append(mdhd, u16(0)...)

	hdlr := make([]byte, 0, 32)
	hdlr = append(hdlr, 0, 0, 0, 0)
	hdlr = append(hdlr, u32(0)...)
	hdlr = append(hdlr, []byte("vide")...)
	hdlr = append(hdlr, make([]byte, 12)...)
	hdlr = append(hdlr, []byte("VideoHandler\x00")...)

	return box("mdia", box("mdhd", mdhd), box("hdlr", hdlr), minf(entry, width, height))
}

func minf(entry []byte, width, height int) []byte {
	vmhd := []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	dref := make([]byte, 0, 28)
	dref = append(dref, 0, 0, 0, 0)
	dref = append(dref, u32(1)...)
	dref = append(dref, box("url ", []byte{0, 0, 0, 1})...)
	dinf := box("dinf", box("dref", dref))
	return box("minf", box("vmhd", vmhd), dinf, stbl(entry))
}

func stbl(entry []byte) []byte {
	stsd := make([]byte, 0, 128)
	stsd = append(stsd, 0, 0, 0, 0)
	stsd = append(stsd, u32(1)...)
	stsd = append(stsd, entry...)

	empty := func(name string) []byte {
		return box(name, []byte{0, 0, 0, 0}, u32(0))
	}
	return box("stbl", box("stsd", stsd), empty("stts"), empty("stsc"),
		box("stsz", []byte{0, 0, 0, 0}, u32(0), u32(0)), empty("stco"))
}

func avc1(info SPSInfo, sps, pps []byte) []byte {
	body := make([]byte, 0, 86)
	body = append(body, make([]byte, 6)...) // gereserveerd
	body = append(body, u16(1)...)          // data reference index
	body = append(body, make([]byte, 16)...)
	body = append(body, u16(uint16(info.Width))...)
	body = append(body, u16(uint16(info.Height))...)
	body = append(body, u32(0x00480000)...) // 72 dpi horizontaal
	body = append(body, u32(0x00480000)...) // en verticaal
	body = append(body, u32(0)...)
	body = append(body, u16(1)...)           // frames per sample
	body = append(body, make([]byte, 32)...) // compressornaam
	body = append(body, u16(0x0018)...)      // kleurdiepte
	body = append(body, 0xFF, 0xFF)          // kleurtabel: geen
	body = append(body, avcC(info, sps, pps)...)
	return box("avc1", body)
}

// avcC draagt de parametersets. Hier staat ook dat lengtes vier bytes zijn,
// wat moet passen bij het voorvoegsel dat WriteFrame schrijft.
func avcC(info SPSInfo, sps, pps []byte) []byte {
	body := make([]byte, 0, 16+len(sps)+len(pps))
	body = append(body, 1, info.ProfileIDC, info.ConstraintSet, info.LevelIDC)
	body = append(body, 0xFF) // zes bits gezet, dan lengte-min-één = 3
	body = append(body, 0xE1) // drie bits gezet, dan één SPS
	body = append(body, u16(uint16(len(sps)))...)
	body = append(body, sps...)
	body = append(body, 1) // één PPS
	body = append(body, u16(uint16(len(pps)))...)
	body = append(body, pps...)
	return box("avcC", body)
}

func mvex() []byte {
	trex := make([]byte, 0, 24)
	trex = append(trex, 0, 0, 0, 0)
	trex = append(trex, u32(1)...) // spoor-id
	trex = append(trex, u32(1)...) // standaard sample description index
	trex = append(trex, u32(0)...)
	trex = append(trex, u32(0)...)
	trex = append(trex, u32(0)...)
	return box("mvex", box("trex", trex))
}

// moof beschrijft één sample. decodeTime is de plek op de tijdlijn die op nul
// begint, niet de tijdstempel van de camera; zie Fragment.
func moof(sequence uint32, decodeTime uint64, duration, size uint32, keyframe bool) []byte {
	mfhd := append([]byte{0, 0, 0, 0}, u32(sequence)...)

	// Alleen default-base-is-moof (0x020000): de data-offset rekent vanaf het begin
	// van deze moof, en er volgen geen standaardwaarden.
	//
	// Hier stond 0x020020, en die 0x20 is default-sample-flags-present -- een veld
	// van vier bytes dat niet geschreven wordt. Precies dezelfde fout als in de
	// trun hieronder, één doosje eerder. ffmpeg klaagt erover ("overread end of
	// atom 'tfhd' by 4 bytes") en leest door; Chrome stopt, en dan mislukt elke
	// volgende appendBuffer met "the HTMLMediaElement.error attribute is not null".
	tfhd := make([]byte, 0, 16)
	tfhd = append(tfhd, 0, 0x02, 0x00, 0x00)
	tfhd = append(tfhd, u32(1)...)

	// Versie 1: 64 bits, want een sessie die lang genoeg openstaat loopt over de
	// 32 bits heen.
	tfdt := append([]byte{1, 0, 0, 0}, u64(decodeTime)...)

	// De offset wijst vanaf het begin van deze moof naar de data: de moof zelf
	// plus de kop van de mdat.
	//
	// De vlaggen moeten precies opsommen wat er ook echt volgt. Hier stond
	// 0x010F01, en dat belooft er één te veel: 0x0800 is de composition-time
	// offset, die niet geschreven wordt. Een lezer rekent zijn veldenlijst uit de
	// vlaggen, komt vier velden per sample tegen waar er drie staan, en loopt het
	// doosje uit. ffprobe leest er dan rommel in als DTS en speelt door; Chrome
	// wijst het fragment af -- geruisloos, want een MediaSource meldt dat alleen
	// aan wie ernaar luistert. Het beeld bleef leeg zonder één foutmelding.
	flags := uint32(0x0000_0701) // data-offset, plus duur, grootte en vlaggen per sample
	trun := make([]byte, 0, 24)
	trun = append(trun, byte(flags>>24), byte(flags>>16), byte(flags>>8), byte(flags))
	trun = append(trun, u32(1)...) // één sample
	offsetPlaceholder := len(trun)
	trun = append(trun, u32(0)...)
	trun = append(trun, u32(duration)...)
	trun = append(trun, u32(size)...)
	if keyframe {
		trun = append(trun, u32(0x0200_0000)...) // hangt van niets af
	} else {
		trun = append(trun, u32(0x0101_0000)...) // hangt af, en niets hangt hiervan af
	}

	traf := box("traf", box("tfhd", tfhd), box("tfdt", tfdt), box("trun", trun))
	fragment := box("moof", box("mfhd", mfhd), traf)
	binary.BigEndian.PutUint32(
		fragment[len(fragment)-len(trun)+offsetPlaceholder:],
		uint32(len(fragment)+8))
	return fragment
}

func u64(value uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], value)
	return out[:]
}

// MimeType is wat een browser moet weten om de MediaSource op te zetten.
func MimeType(info SPSInfo) string {
	return fmt.Sprintf("video/mp4; codecs=\"avc1.%02X%02X%02X\"",
		info.ProfileIDC, info.ConstraintSet, info.LevelIDC)
}
