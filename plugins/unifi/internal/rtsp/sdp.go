package rtsp

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// De SDP zegt wat er te halen is.
//
// Wij zoeken er vier dingen in: welk spoor het beeld draagt (m=video), met welk
// nummer het in de pakketten staat, waar het te vinden is (a=control), en wat
// het is (a=rtpmap).
//
// Bij H.264 staan de parametersets erbij, in a=fmtp;sprop-parameter-sets, en
// die beschrijven het beeld -- zonder die twee kan geen speler iets met de
// frames erna. Bij AV1 staan ze er niet: daar komt de sequence header in de
// stroom zelf mee.
//
// Een camera stuurt vaak meerdere sporen over dezelfde verbinding -- hier AAC,
// Opus en beeld. Vandaar het nummer: zonder dat lees je geluid als beeld.

// Codec is wat het videospoor draagt.
type Codec uint8

const (
	// H264 komt van de oudere camera's. De parametersets staan in de SDP.
	H264 Codec = iota
	// AV1 komt van de nieuwere. Er staan geen parametersets in de SDP: de
	// sequence header komt in de stroom zelf mee, en die is er dus pas als het
	// eerste beeld binnen is.
	AV1
)

// Media beschrijft het videospoor.
type Media struct {
	Codec   Codec
	Control string
	// SPS en PPS zijn alleen bij H.264 gevuld.
	SPS, PPS []byte
	// Payload is het nummer waarmee dit spoor in de RTP-pakketten staat. Nodig
	// omdat een camera meerdere sporen op dezelfde verbinding stuurt.
	Payload uint8
}

func parseSDP(body string) (Media, error) {
	var media Media
	found := false
	inVideo := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			// Een nieuwe mediasectie: alles hierna hoort daarbij, tot de
			// volgende. Een camera biedt vaak ook geluid aan, en de a=control
			// daarvan zou het beeld naar het verkeerde spoor sturen.
			inVideo = strings.HasPrefix(line, "m=video")
			if inVideo {
				media.Payload = payloadType(line)
			}
		case inVideo && strings.HasPrefix(line, "a=control:"):
			media.Control = strings.TrimSpace(strings.TrimPrefix(line, "a=control:"))
		case inVideo && strings.HasPrefix(line, "a=rtpmap:"):
			switch {
			case strings.Contains(line, "H264"):
				media.Codec, found = H264, true
			case strings.Contains(line, "AV1"):
				media.Codec, found = AV1, true
			}
		case inVideo && strings.HasPrefix(line, "a=fmtp:"):
			media.SPS, media.PPS = parameterSets(line)
		}
	}
	if !found {
		return Media{}, fmt.Errorf("rtsp: the camera offers no video track this can read; " +
			"only H.264 and AV1 are supported")
	}
	if media.Codec == H264 && (len(media.SPS) == 0 || len(media.PPS) == 0) {
		return Media{}, fmt.Errorf("rtsp: the camera announces H.264 but sends no parameter sets")
	}
	return media, nil
}

// payloadType leest het nummer uit een m=-regel: "m=video 0 RTP/AVP 97".
func payloadType(line string) uint8 {
	parts := strings.Fields(line)
	if len(parts) < 4 {
		return 0
	}
	value, err := strconv.Atoi(parts[3])
	if err != nil || value < 0 || value > 127 {
		return 0
	}
	return uint8(value)
}

// parameterSets leest sprop-parameter-sets, dat twee base64-stukken draagt met
// een komma ertussen.
func parameterSets(line string) (sps, pps []byte) {
	_, parameters, found := strings.Cut(line, " ")
	if !found {
		return nil, nil
	}
	for _, parameter := range strings.Split(parameters, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
		if !found || name != "sprop-parameter-sets" {
			continue
		}
		parts := strings.Split(value, ",")
		if len(parts) < 2 {
			return nil, nil
		}
		sps, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(parts[0]))
		pps, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
		return sps, pps
	}
	return nil, nil
}
