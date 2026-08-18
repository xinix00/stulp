package rtsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Tegen een echte camera.
//
// Draait alleen als STULP_RTSP_URL gezet is, want een test hoort niet stil af te
// hangen van hardware die er meestal niet is. Wat hier getoetst wordt is precies
// wat een nagebouwde server niet kan bewijzen: dat een echte camera zijn
// beschrijving stuurt zoals wij hem lezen, en dat er beeld uit komt.
func TestAgainstARealCamera(t *testing.T) {
	address := os.Getenv("STULP_RTSP_URL")
	if address == "" {
		t.Skip("geen STULP_RTSP_URL; deze toets vraagt een echte camera")
	}
	stream, err := Dial(address, 20*time.Second)
	if err != nil {
		t.Fatalf("verbinden met de camera: %v", err)
	}
	defer stream.Close()
	t.Logf("  codec: %v (0=H264, 1=AV1), spoor %s", stream.Codec(), stream.Media.Control)

	dir := os.Getenv("STULP_RTSP_OUT")
	if dir == "" {
		dir = t.TempDir()
	}
	target := filepath.Join(dir, "camera.mp4")
	file, err := os.Create(target)
	if err != nil {
		t.Fatal(err)
	}

	var h264 Assembler
	var av1 AV1Assembler
	var muxer *Muxer
	frames, keyframes, written, packets := 0, 0, 0, 0
	deadline := time.Now().Add(25 * time.Second)
	for frames < 30 && time.Now().Before(deadline) {
		packet, err := stream.ReadPacket(10 * time.Second)
		if err != nil {
			break
		}
		if packets < 12 {
			t.Logf("    pakket %2d: ts=%d marker=%v len=%d agg=0x%02X",
				packets, packet.Timestamp, packet.Marker, len(packet.Payload), packet.Payload[0])
		}
		packets++
		var unit [][]byte
		var timestamp uint32
		var complete bool
		if stream.Codec() == AV1 {
			unit, timestamp, complete = av1.Push(packet)
		} else {
			unit, timestamp, complete = h264.Push(packet)
		}
		if !complete {
			continue
		}
		if muxer == nil {
			// Bij AV1 kan het doosje pas als de sequence header langs is
			// geweest; bij H.264 stond alles al in de beschrijving.
			if stream.Codec() == AV1 {
				sequence := av1.SequenceHeader()
				if sequence == nil {
					continue
				}
				muxer, err = NewAV1Muxer(sequence)
			} else {
				muxer, err = NewMuxer(stream.SPS(), stream.PPS())
			}
			if err != nil {
				t.Fatalf("muxer: %v", err)
			}
			t.Logf("  %s, kopdeel %d bytes", muxer.MimeType(), len(muxer.Header()))
			if _, err := file.Write(muxer.Header()); err != nil {
				t.Fatal(err)
			}
		}
		if muxer.Keyframe(unit) {
			keyframes++
		}
		if frames < 4 {
			kinds := make([]int, 0, len(unit))
			for _, obu := range unit {
				kinds = append(kinds, int((obu[0]>>3)&0x0F))
			}
			t.Logf("    frame %d: obu-types %v, tijdstempel %d", frames, kinds, timestamp)
		}
		fragment, err := muxer.Fragment(unit, timestamp)
		if err != nil {
			t.Fatalf("fragment %d: %v", frames, err)
		}
		if _, err := file.Write(fragment); err != nil {
			t.Fatal(err)
		}
		written += len(fragment)
		frames++
	}
	file.Close()
	if frames < 10 {
		t.Fatalf("maar %d frames uit de camera", frames)
	}
	if keyframes == 0 {
		t.Fatal("geen enkel keyframe; een speler kan dan nergens beginnen")
	}
	t.Logf("  %d frames, %d keyframes, %d bytes", frames, keyframes, written)

	// En dan het echte oordeel: kan een speler het openen.
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		out, err := exec.Command(ffprobe, "-v", "error", "-count_frames",
			"-show_entries", "stream=codec_name,width,height,nb_read_frames",
			"-of", "default=noprint_wrappers=1", target).CombinedOutput()
		t.Logf("  ffprobe:\n%s", out)
		if err != nil {
			t.Fatalf("ffprobe kon het bestand niet lezen: %v", err)
		}
	}
}

// De beschrijving die een echte camera stuurt, onbewerkt. Dit is het bewijs
// waarop de SDP-lezer gebouwd wordt: wat er staat, niet wat er zou moeten staan.
func TestDumpRealSDP(t *testing.T) {
	address := os.Getenv("STULP_RTSP_URL")
	if address == "" {
		t.Skip("geen STULP_RTSP_URL")
	}
	body, err := describe(address, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", body)
}
