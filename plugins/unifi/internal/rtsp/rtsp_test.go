package rtsp

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// Een echte SDP van een UniFi-camera, ingekort tot wat wij eruit lezen.
const sampleSDP = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=Session
m=audio 0 RTP/AVP 97
a=control:trackID=1
a=rtpmap:97 mpeg4-generic/16000
m=video 0 RTP/AVP 96
a=control:trackID=0
a=rtpmap:96 H264/90000
a=fmtp:96 packetization-mode=1;profile-level-id=4D0029;sprop-parameter-sets=Z00AKZpmA8ARPy4C3AQEBQAAAwPoAADqYOhgBGMAAF9eC7y40MAjGAAC+vBd5caCAA==,aO48gA==
`

// De SDP wijst het videospoor aan, niet het eerste spoor dat langskomt. Een
// camera biedt ook geluid aan, en dat heeft zijn eigen a=control.
func TestSDPPicksTheVideoTrack(t *testing.T) {
	media, err := parseSDP(sampleSDP)
	if err != nil {
		t.Fatal(err)
	}
	control, sps, pps := media.Control, media.SPS, media.PPS
	if control != "trackID=0" {
		t.Fatalf("spoor = %q, wil het videospoor trackID=0", control)
	}
	if len(sps) == 0 || len(pps) == 0 {
		t.Fatalf("parametersets = %d en %d bytes", len(sps), len(pps))
	}
	if sps[0]&0x1F != 7 {
		t.Fatalf("de eerste set is geen SPS maar type %d", sps[0]&0x1F)
	}
	if pps[0]&0x1F != 8 {
		t.Fatalf("de tweede set is geen PPS maar type %d", pps[0]&0x1F)
	}
}

// Een stream zonder parametersets is niet af te spelen. Dat hoort te falen met
// een melding die zegt wat er aan de hand is.
func TestSDPWithoutParameterSetsFailsClearly(t *testing.T) {
	_, err := parseSDP("v=0\nm=video 0 RTP/AVP 96\na=control:trackID=0\na=rtpmap:96 H264/90000\n")
	if err == nil || !strings.Contains(err.Error(), "parameter sets") {
		t.Fatalf("H.264 zonder parametersets gaf %v", err)
	}
	// En een codec die deze lezer niet kent hoort te zeggen wat hij wél kan.
	_, err = parseSDP("v=0\nm=video 0 RTP/AVP 96\na=rtpmap:96 H265/90000\n")
	if err == nil || !strings.Contains(err.Error(), "H.264 and AV1") {
		t.Fatalf("H.265 gaf %v", err)
	}
}

// De afmeting komt uit de SPS en nergens anders vandaan.
func TestSPSCarriesThePictureSize(t *testing.T) {
	media, err := parseSDP(sampleSDP)
	if err != nil {
		t.Fatal(err)
	}
	sps := media.SPS
	info, err := ParseSPS(sps)
	if err != nil {
		t.Fatal(err)
	}
	// Onafhankelijk nagerekend uit dezelfde parameterset, en bevestigd door
	// ffprobe op een bestand dat deze muxer geschreven heeft.
	if info.Width != 1920 || info.Height != 1080 {
		t.Fatalf("afmeting = %dx%d, wil 1920x1080", info.Width, info.Height)
	}
	if got := MimeType(info); got != `video/mp4; codecs="avc1.4D0029"` {
		t.Fatalf("mime = %q", got)
	}
}

func rtpPacket(timestamp uint32, marker bool, payload []byte) Packet {
	return Packet{Timestamp: timestamp, Marker: marker, Payload: payload}
}

// Eén NAL over meerdere pakketten: zonder dit valt elk beeldframe uit elkaar,
// want een frame past niet in één netwerkpakket.
func TestFUAFragmentsBecomeOneNAL(t *testing.T) {
	var assembler Assembler
	// FU-A: kop 0x7C (type 28, bovenste bits van de oorspronkelijke NAL),
	// dan start/eind plus het echte type 5 (keyframe).
	assembler.Push(rtpPacket(1000, false, []byte{0x7C, 0x85, 'a', 'b'}))
	assembler.Push(rtpPacket(1000, false, []byte{0x7C, 0x05, 'c'}))
	unit, timestamp, ok := assembler.Push(rtpPacket(1000, true, []byte{0x7C, 0x45, 'd'}))
	if !ok {
		t.Fatal("het frame kwam nooit af")
	}
	if timestamp != 1000 || len(unit) != 1 {
		t.Fatalf("eenheid = %d NALs op %d", len(unit), timestamp)
	}
	if got := string(unit[0][1:]); got != "abcd" {
		t.Fatalf("samengevoegd = %q, wil abcd", got)
	}
	if unit[0][0]&0x1F != 5 {
		t.Fatalf("de NAL-kop is verloren gegaan: 0x%02X", unit[0][0])
	}
	if !IsKeyframe(unit) {
		t.Fatal("een type-5-NAL werd niet als keyframe herkend")
	}
}

// STAP-A draagt meerdere NALs in één pakket: zo komen SPS en PPS mee.
func TestSTAPASplitsIntoSeveralNALs(t *testing.T) {
	payload := []byte{24} // STAP-A
	for _, nal := range [][]byte{{0x67, 1, 2}, {0x68, 3}} {
		payload = append(payload, byte(len(nal)>>8), byte(len(nal)))
		payload = append(payload, nal...)
	}
	var assembler Assembler
	unit, _, ok := assembler.Push(rtpPacket(90, true, payload))
	if !ok || len(unit) != 2 {
		t.Fatalf("STAP-A gaf %d NALs (ok=%v)", len(unit), ok)
	}
	if unit[0][0] != 0x67 || unit[1][0] != 0x68 {
		t.Fatalf("verkeerde NALs: %x %x", unit[0], unit[1])
	}
}

// Een camera die de marker niet zet moet toch frames opleveren: het verspringen
// van de tijdstempel is dan het einde.
func TestTimestampChangeEndsAFrameWithoutAMarker(t *testing.T) {
	var assembler Assembler
	assembler.Push(rtpPacket(100, false, []byte{0x41, 'x'}))
	unit, timestamp, ok := assembler.Push(rtpPacket(200, false, []byte{0x41, 'y'}))
	if !ok || timestamp != 100 || len(unit) != 1 {
		t.Fatalf("frame bij tijdsprong = %d NALs op %d (ok=%v)", len(unit), timestamp, ok)
	}
}

// Een fragment waarvan het begin gemist is levert geen halve NAL op. Beter niets
// dan een frame dat de speler laat struikelen.
func TestFUAWithoutItsStartIsDropped(t *testing.T) {
	var assembler Assembler
	unit, _, ok := assembler.Push(rtpPacket(1, true, []byte{0x7C, 0x45, 'x'}))
	if ok && len(unit) > 0 {
		t.Fatalf("een fragment zonder begin werd toch doorgegeven: %x", unit)
	}
}

// Het RTP-pakket zelf: de kop kan een uitbreiding dragen en die hoort niet in
// de lading terecht te komen.
func TestRTPSkipsItsHeaderAndExtension(t *testing.T) {
	packet := make([]byte, 0, 32)
	packet = append(packet, 0x90, 0xE0)       // versie 2, uitbreiding, marker
	packet = append(packet, 0, 1)             // volgnummer
	packet = append(packet, 0, 0, 0x1F, 0x40) // tijdstempel 8000
	packet = append(packet, 0, 0, 0, 0)       // ssrc
	packet = append(packet, 0xBE, 0xDE, 0, 1) // uitbreiding: één woord
	packet = append(packet, 1, 2, 3, 4)
	packet = append(packet, 'l', 'a', 'd', 'i', 'n', 'g')

	got, ok := parseRTP(packet)
	if !ok {
		t.Fatal("geldig pakket werd geweigerd")
	}
	if got.Timestamp != 8000 || !got.Marker {
		t.Fatalf("kop = %+v", got)
	}
	if string(got.Payload) != "lading" {
		t.Fatalf("lading = %q", got.Payload)
	}
}

// Het doosje moet kloppen: een browser die een onbekende doos tegenkomt speelt
// niets, en dat merk je anders pas in de browser.
func TestMuxerWritesAReadableContainer(t *testing.T) {
	media, err := parseSDP(sampleSDP)
	if err != nil {
		t.Fatal(err)
	}
	sps, pps := media.SPS, media.PPS
	muxer, err := NewMuxer(sps, pps)
	if err != nil {
		t.Fatal(err)
	}
	header := muxer.Header()
	for _, want := range []string{"ftyp", "moov", "mvhd", "trak", "avc1", "avcC", "mvex", "trex"} {
		if !bytes.Contains(header, []byte(want)) {
			t.Errorf("het kopdeel mist %q", want)
		}
	}
	if err := walkBoxes(header); err != nil {
		t.Fatalf("kopdeel: %v", err)
	}

	fragment, err := muxer.Fragment([][]byte{{0x65, 1, 2, 3}}, 90000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"moof", "mfhd", "traf", "tfhd", "tfdt", "trun", "mdat"} {
		if !bytes.Contains(fragment, []byte(want)) {
			t.Errorf("het fragment mist %q", want)
		}
	}
	if err := walkBoxes(fragment); err != nil {
		t.Fatalf("fragment: %v", err)
	}
}

// De data-offset in trun moet precies op de inhoud van de mdat wijzen. Staat
// hij ernaast, dan leest de speler rommel als beeld.
func TestFragmentDataOffsetPointsAtTheSample(t *testing.T) {
	media, _ := parseSDP(sampleSDP)
	sps, pps := media.SPS, media.PPS
	muxer, err := NewMuxer(sps, pps)
	if err != nil {
		t.Fatal(err)
	}
	sample := []byte{0x65, 9, 9, 9, 9, 9}
	fragment, err := muxer.Fragment([][]byte{sample}, 90000)
	if err != nil {
		t.Fatal(err)
	}

	moofSize := binary.BigEndian.Uint32(fragment[0:4])
	trun := bytes.Index(fragment, []byte("trun"))
	if trun < 0 {
		t.Fatal("geen trun gevonden")
	}
	// Na de naam: vlaggen (4), aantal samples (4), dan de offset.
	offset := binary.BigEndian.Uint32(fragment[trun+4+4+4 : trun+4+4+4+4])
	if offset != moofSize+8 {
		t.Fatalf("data-offset = %d, wil %d (moof %d plus de mdat-kop)", offset, moofSize+8, moofSize)
	}
	// En daar moet de lengte van de NAL staan, niet iets anders.
	if got := binary.BigEndian.Uint32(fragment[offset : offset+4]); got != uint32(len(sample)) {
		t.Fatalf("op de offset staat %d, wil de NAL-lengte %d", got, len(sample))
	}
}

// De vlaggen van tfhd en trun moeten precies opsommen wat er in het doosje staat.
//
// Dit ging twee keer mis en het kostte een avond. De trun beloofde een
// composition-time offset die er niet was, en de tfhd een default-sample-flags
// die er niet was. ffmpeg zegt daarvan "overread end of atom" en leest gewoon
// door -- de container leek dus in orde, en ffprobe decodeerde alle frames --
// maar Chrome rekent zijn veldenlijst uit diezelfde vlaggen, loopt het doosje uit
// en wijst het fragment af. Een MediaSource meldt dat alleen aan wie ernaar
// luistert, dus bleef er een leeg venster over zonder één foutmelding.
//
// De test rekent de maat uit de vlaggen in plaats van een waarde vast te leggen,
// zodat hij standhoudt als er ooit een veld bij komt.
func TestFragmentBoxesPromiseExactlyWhatTheyCarry(t *testing.T) {
	media, _ := parseSDP(sampleSDP)
	muxer, err := NewMuxer(media.SPS, media.PPS)
	if err != nil {
		t.Fatal(err)
	}
	// De velden die elke vlag toevoegt, uit ISO/IEC 14496-12. Alles wat niet in
	// deze tabel staat bestaat niet en hoort dus niet gezet te zijn.
	tfhdFields := map[uint32]uint32{0x000001: 8, 0x000002: 4, 0x000008: 4, 0x000010: 4, 0x000020: 4, 0x010000: 0, 0x020000: 0}
	trunOnce := map[uint32]uint32{0x000001: 4, 0x000004: 4}
	trunPerSample := map[uint32]uint32{0x000100: 4, 0x000200: 4, 0x000400: 4, 0x000800: 4}

	// Een keyframe en een gewoon beeld: die zetten verschillende sample-vlaggen.
	for _, unit := range [][][]byte{{{0x65, 1, 2, 3}}, {{0x41, 4, 5}}} {
		fragment, err := muxer.Fragment(unit, 90000)
		if err != nil {
			t.Fatal(err)
		}
		box := func(name string) (size, flags uint32, payload []byte) {
			at := bytes.Index(fragment, []byte(name))
			if at < 0 {
				t.Fatalf("geen %s gevonden", name)
			}
			size = binary.BigEndian.Uint32(fragment[at-4 : at])
			flags = uint32(fragment[at+5])<<16 | uint32(fragment[at+6])<<8 | uint32(fragment[at+7])
			return size, flags, fragment[at+4 : at-4+int(size)]
		}
		check := func(name string, size, flags, want, known uint32) {
			if got := size - 8; got != want {
				t.Fatalf("%s draagt %d bytes maar vlaggen 0x%06x beloven er %d", name, got, flags, want)
			}
			if unknown := flags & ^known; unknown != 0 {
				t.Fatalf("%s zet vlaggen die niet bestaan: 0x%06x", name, unknown)
			}
		}

		// tfhd: versie en vlaggen (4) plus het spoor-id (4), en daarna wat de
		// vlaggen beloven.
		size, flags, _ := box("tfhd")
		want, known := uint32(8), uint32(0)
		for bit, width := range tfhdFields {
			known |= bit
			if flags&bit != 0 {
				want += width
			}
		}
		check("tfhd", size, flags, want, known)

		// trun: versie en vlaggen (4) plus het aantal samples (4), dan de velden
		// die één keer voorkomen, en tot slot per sample.
		size, flags, payload := box("trun")
		count := binary.BigEndian.Uint32(payload[4:8])
		want, known = 8, 0
		for bit, width := range trunOnce {
			known |= bit
			if flags&bit != 0 {
				want += width
			}
		}
		perSample := uint32(0)
		for bit, width := range trunPerSample {
			known |= bit
			if flags&bit != 0 {
				perSample += width
			}
		}
		check("trun", size, flags, want+count*perSample, known)
	}
}

// decodeTimeOf leest de baseMediaDecodeTime uit een fragment: waar dit beeld op
// de tijdlijn staat.
func decodeTimeOf(t *testing.T, fragment []byte) uint64 {
	t.Helper()
	tfdt := bytes.Index(fragment, []byte("tfdt"))
	if tfdt < 0 {
		t.Fatal("geen tfdt gevonden")
	}
	if version := fragment[tfdt+4]; version != 1 {
		t.Fatalf("tfdt is versie %d; versie 1 hoort erbij, want 32 bits lopen over", version)
	}
	// Na de naam: versie (1) en vlaggen (3), dan de tijd in 64 bits.
	return binary.BigEndian.Uint64(fragment[tfdt+8 : tfdt+16])
}

// De tijdlijn moet op nul beginnen, wat de camera ook als tijdstempel stuurt.
//
// Dit ging mis tegen een echte camera. RTP begint op een willekeurige waarde
// (RFC 3550), en die ging ongewijzigd de tfdt in: het beeld van de voordeur kwam
// binnen op 4218945853, wat bij 90 kHz dertien uur is. Een browser begint op nul,
// vindt daar niets, en wacht -- zonder fout, want er is niets kapot. In de
// interface bleef "Verbinden…" staan tot je het venster sloot.
func TestTheTimelineStartsAtZeroWhateverTheCameraSends(t *testing.T) {
	media, _ := parseSDP(sampleSDP)
	muxer, err := NewMuxer(media.SPS, media.PPS)
	if err != nil {
		t.Fatal(err)
	}
	// Precies de tijdstempel die de echte camera stuurde.
	first, err := muxer.Fragment([][]byte{{0x65, 1, 2, 3}}, 4218945853)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeTimeOf(t, first); got != 0 {
		t.Fatalf("het eerste beeld staat op %d en moet op 0 staan", got)
	}
	second, err := muxer.Fragment([][]byte{{0x41, 4}}, 4218945853+3000)
	if err != nil {
		t.Fatal(err)
	}
	// Het verschil blijft wat de camera zei: drieduizend eenheden is een dertigste
	// seconde bij 90 kHz.
	if got := decodeTimeOf(t, second); got != 3000 {
		t.Fatalf("het tweede beeld staat op %d en moet op 3000 staan", got)
	}
}

// Na dertien uur kijken loopt de 32-bits teller om. De tijdlijn mag dan niet
// terugspringen, want een speler gooit alles weg wat vóór ligt op wat hij al
// heeft.
func TestTheTimelineKeepsGoingWhenTheCounterWraps(t *testing.T) {
	media, _ := parseSDP(sampleSDP)
	muxer, err := NewMuxer(media.SPS, media.PPS)
	if err != nil {
		t.Fatal(err)
	}
	// Beginnen net onder de overloop, en er dan twee keer 3000 bij optellen: het
	// derde beeld komt bij de camera als een klein getal binnen.
	const start = uint32(0xFFFF_F000)
	// Als variabele, want als constante zou de overloop een compilerfout zijn --
	// terwijl dat precies is wat de teller in het echt doet.
	wrapped := start + 3000
	wrapped += 3000
	for index, timestamp := range []uint32{start, start + 3000, wrapped} {
		fragment, err := muxer.Fragment([][]byte{{0x41, byte(index)}}, timestamp)
		if err != nil {
			t.Fatal(err)
		}
		want := uint64(index) * 3000
		if got := decodeTimeOf(t, fragment); got != want {
			t.Fatalf("beeld %d staat op %d en moet op %d staan (ruwe tijdstempel %d)",
				index, got, want, timestamp)
		}
	}
}

// walkBoxes loopt de doosjes langs en toetst dat elke lengte klopt: een doos die
// buiten zijn ouder valt is een bestand dat niemand kan lezen.
func walkBoxes(data []byte) error {
	for offset := 0; offset < len(data); {
		if offset+8 > len(data) {
			return errAt(offset, "een doos zonder kop")
		}
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		name := string(data[offset+4 : offset+8])
		if size < 8 || offset+size > len(data) {
			return errAt(offset, "doos "+name+" beweert een lengte die niet past")
		}
		if name == "moov" || name == "trak" || name == "mdia" || name == "minf" ||
			name == "stbl" || name == "dinf" || name == "moof" || name == "traf" || name == "mvex" {
			if err := walkBoxes(data[offset+8 : offset+size]); err != nil {
				return err
			}
		}
		offset += size
	}
	return nil
}

type boxError struct {
	offset  int
	message string
}

func (e boxError) Error() string {
	return e.message + " op positie " + itoa(e.offset)
}

func errAt(offset int, message string) error { return boxError{offset: offset, message: message} }

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// Twee beelden achter elkaar, elk afgesloten met een marker. Het tweede moet
// zijn eigen tijd dragen.
//
// Dit ging mis tegen een echte camera: na een marker bleef de tijdstempel op die
// van het vorige beeld staan, en dan zet een speler alles op hetzelfde moment --
// wat neerkomt op één stilstaand beeld in plaats van een film.
func TestEachFrameCarriesItsOwnTime(t *testing.T) {
	var assembler Assembler
	_, first, ok := assembler.Push(rtpPacket(1000, true, []byte{0x65, 'a'}))
	if !ok || first != 1000 {
		t.Fatalf("eerste beeld op %d", first)
	}
	_, second, ok := assembler.Push(rtpPacket(4600, true, []byte{0x41, 'b'}))
	if !ok {
		t.Fatal("tweede beeld kwam niet af")
	}
	if second != 4600 {
		t.Fatalf("tweede beeld draagt %d, wil 4600 -- de tijd van zijn voorganger", second)
	}
}

// Hetzelfde voor AV1, waar het gevonden is.
func TestAV1FramesCarryTheirOwnTime(t *testing.T) {
	var assembler AV1Assembler
	// Aggregatiekop 0x00: één stuk met lengte, geen vervolg.
	frame := func(timestamp uint32) Packet {
		obu := []byte{0x30, 'x'} // type 6 (frame), zonder lengteveld
		payload := append([]byte{0x00}, writeLEB128(uint64(len(obu)))...)
		payload = append(payload, obu...)
		return Packet{Timestamp: timestamp, Marker: true, Payload: payload}
	}
	if _, first, ok := assembler.Push(frame(1000)); !ok || first != 1000 {
		t.Fatalf("eerste beeld op %d (ok=%v)", first, ok)
	}
	_, second, ok := assembler.Push(frame(4600))
	if !ok || second != 4600 {
		t.Fatalf("tweede beeld draagt %d (ok=%v), wil 4600", second, ok)
	}
}

// Een sequence header is geen bewijs van een keyframe.
//
// Dit stond er wél zo in, en een echte UniFi-camera weerlegt het: die stuurt de
// sequence header bij elk beeld mee. Daardoor gold het allereerste beeld altijd
// als instappunt, begon de stream op een INTER-beeld, en zag een speler nooit
// iets -- er was geen referentie om op te bouwen. In een opname van de voordeur
// was geen van de eerste tien beelden een keyframe.
func TestASequenceHeaderDoesNotMakeAKeyframe(t *testing.T) {
	// obu bouwt een OBU met lengteveld: kop, lengte, inhoud.
	obu := func(kind byte, payload ...byte) []byte {
		head := []byte{kind<<3 | 0x02}
		head = append(head, writeLEB128(uint64(len(payload)))...)
		return append(head, payload...)
	}
	// De beeldkop: bit 7 is show_existing_frame, bit 6-5 zijn frame_type.
	frameHeader := func(showExisting bool, frameType byte) byte {
		var first byte
		if showExisting {
			first |= 0x80
		}
		return first | frameType<<5
	}

	sequence := obu(obuSequenceHeader, 0x00, 0x00)

	for _, test := range []struct {
		name string
		unit [][]byte
		want bool
	}{
		{"keyframe zonder sequence header",
			[][]byte{obu(obuFrame, frameHeader(false, 0), 'x')}, true},
		{"keyframe met sequence header",
			[][]byte{sequence, obu(obuFrame, frameHeader(false, 0), 'x')}, true},
		{"inter-beeld met sequence header -- precies wat de camera stuurt",
			[][]byte{sequence, obu(obuFrame, frameHeader(false, 1), 'x')}, false},
		{"intra-only telt niet: het leunt op de referentielijst",
			[][]byte{obu(obuFrame, frameHeader(false, 2), 'x')}, false},
		{"switch-beeld telt ook niet",
			[][]byte{obu(obuFrame, frameHeader(false, 3), 'x')}, false},
		{"een hertoond beeld is nooit een startpunt",
			[][]byte{obu(obuFrame, frameHeader(true, 0), 'x')}, false},
		{"een losse beeldkop telt net zo goed",
			[][]byte{obu(obuFrameHeader, frameHeader(false, 0), 'x')}, true},
		{"alleen een sequence header is geen beeld",
			[][]byte{sequence}, false},
	} {
		if got := IsAV1Keyframe(test.unit); got != test.want {
			t.Errorf("%s: kreeg %v, wil %v", test.name, got, test.want)
		}
	}
}
