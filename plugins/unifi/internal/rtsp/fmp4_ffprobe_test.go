package rtsp

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Het doosje door een echte speler laten beoordelen.
//
// Onze eigen tests toetsen dat de doosjes kloppen zoals wij ze bedoelen. Dat is
// niet hetzelfde als "een speler kan het openen": een lengte die overal
// consistent fout is blijft consistent. ffmpeg schrijft hier echte H.264, onze
// muxer maakt er een fMP4 van, en ffprobe zegt wat hij ziet.
//
// De test slaat zichzelf over als ffmpeg er niet is. Dat is geen ontsnapping --
// hij draait waar hij kan, en waar hij niet kan blijven de andere tests over.
func TestContainerIsReadableByARealPlayer(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe staat niet op deze machine")
	}
	target := muxedTestFile(t)

	probe := exec.Command(ffprobe, "-v", "error", "-count_frames",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames",
		"-of", "json", target.path)
	output, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe kon het bestand niet lezen: %v\n%s", err, output)
	}
	var report struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Frames    string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("ffprobe gaf %s", output)
	}
	if len(report.Streams) != 1 {
		t.Fatalf("ffprobe zag %d sporen", len(report.Streams))
	}
	stream := report.Streams[0]
	if stream.CodecName != "h264" || stream.Width != 320 || stream.Height != 240 {
		t.Fatalf("ffprobe ziet %s %dx%d", stream.CodecName, stream.Width, stream.Height)
	}
	// Elk frame moet ook echt te decoderen zijn. Een doosje dat opent maar
	// waarvan de inhoud niet klopt geeft hier nul.
	if strings.TrimSpace(stream.Frames) != itoa(len(target.units)) {
		t.Fatalf("ffprobe decodeerde %s van de %d frames", stream.Frames, len(target.units))
	}
}

// Een speler die klaagt maar doorleest is geen speler die het goedkeurt.
//
// Dit is de test die er niet was, en dat heeft twee keer geld gekost. Zowel de
// tfhd als de trun beloofden in hun vlaggen een veld dat er niet stond. ffmpeg
// zegt daarvan "overread end of atom", leest gewoon door en decodeert alle frames
// -- dus stond de test hierboven op groen. Chrome stopt er wél mee, geruisloos,
// en dan blijft er een leeg venster over.
//
// Vandaar: op waarschuwingsniveau lezen en elke klacht als fout rekenen. Een
// doosje dat klopt levert geen enkele regel op stderr.
func TestNoPlayerComplainsAboutTheBoxes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg staat niet op deze machine")
	}
	target := muxedTestFile(t)

	// -f null: alles decoderen en weggooien. Het gaat hier om wat de lezer
	// onderweg te melden heeft, niet om het resultaat.
	decode := exec.Command(ffmpeg, "-v", "warning", "-i", target.path, "-f", "null", "-")
	var complaints bytes.Buffer
	decode.Stderr = &complaints
	if err := decode.Run(); err != nil {
		t.Fatalf("ffmpeg kon het bestand niet decoderen: %v\n%s", err, complaints.String())
	}
	if said := strings.TrimSpace(complaints.String()); said != "" {
		t.Fatalf("een speler heeft klachten over de doosjes:\n%s", said)
	}
}

// muxedTestFile maakt echt beeld met ffmpeg, laat onze muxer er een fMP4 van
// maken en geeft het bestand terug.
type muxedFile struct {
	path  string
	units [][][]byte
}

func muxedTestFile(t *testing.T) muxedFile {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg staat niet op deze machine")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "bron.h264")
	// -bf 0: geen B-frames. Dat is wat een camera in een livestream stuurt, en het
	// is ook het enige wat deze muxer kan uitdrukken -- hij schrijft geen
	// composition-time offsets, dus presentatie- en decodeervolgorde zijn hier
	// hetzelfde. Zonder deze vlag maakt libx264 er 34 B-frames in en klaagt ffmpeg
	// over de volgorde, wat een echte beperking is maar niet degene die deze test
	// moet bewaken.
	encode := exec.Command(ffmpeg, "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "main", "-level", "3.1",
		"-g", "25", "-bf", "0", "-f", "h264", source, "-y")
	if out, err := encode.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg kon geen testbeeld maken: %v %s", err, out)
	}

	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	sps, pps, units := splitAnnexB(raw)
	if len(sps) == 0 || len(pps) == 0 || len(units) < 10 {
		t.Fatalf("testbeeld leverde %d parametersets en %d frames", len(sps)+len(pps), len(units))
	}

	target := filepath.Join(dir, "uit.mp4")
	file, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}
	muxer, err := NewMuxer(sps, pps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(muxer.Header()); err != nil {
		t.Fatal(err)
	}
	for index, unit := range units {
		// 90 kHz gedeeld door 25 beelden: 3600 tikken per frame.
		fragment, err := muxer.Fragment(unit, uint32(index)*3600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(fragment); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	return muxedFile{path: target, units: units}
}

// splitAnnexB haalt de parametersets en de frames uit een stroom met startcodes.
func splitAnnexB(raw []byte) (sps, pps []byte, units [][][]byte) {
	var current [][]byte
	for _, part := range bytes.Split(raw, []byte{0, 0, 1}) {
		part = bytes.TrimSuffix(part, []byte{0})
		if len(part) == 0 {
			continue
		}
		switch part[0] & 0x1F {
		case 7:
			sps = part
		case 8:
			pps = part
		case 1, 5:
			if len(current) > 0 {
				units = append(units, current)
			}
			current = [][]byte{part}
		default:
			if len(current) > 0 {
				current = append(current, part)
			}
		}
	}
	if len(current) > 0 {
		units = append(units, current)
	}
	return sps, pps, units
}
