package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/stulp/plugins/unifi/internal/rtsp"
)

// De camera bedient zijn eigen beeld.
//
// Stulp geeft alleen bytes door en weet niets van codecs. Het ompakken hoort
// hier: deze app weet dat een UniFi-camera RTSPS met H.264 spreekt, en niemand
// anders hoeft dat te weten. Een installatie zonder camera's draagt geen regel
// van deze code.
//
// De luisteraar staat op localhost. Hij hoeft niet vanaf de browser bereikbaar
// te zijn -- Stulp haalt op en geeft door -- en zo is er geen poort in huis
// waar beeld uit komt zonder dat er iets over gaat.

type streamHost struct {
	mu       sync.Mutex
	listener net.Listener
	sessions map[string]*session
}

// session is één camera die bekeken wordt.
type session struct {
	mu      sync.Mutex
	viewers map[*viewer]struct{}
	header  []byte
	// mime is het volledige mediatype, inclusief codec: video/mp4; codecs="av01…".
	// Een browser heeft dat nodig vóór hij de eerste byte ziet -- Media Source
	// Extensions maakt zijn buffer op die string, en "video/mp4" alleen is niet
	// genoeg om te weten of hij het aankan.
	mime string
	stop context.CancelFunc

	// gop draagt de fragmenten sinds het laatste keyframe.
	//
	// Zonder dit begint een kijker op het beeld dat toevallig langskomt, en dat
	// verwijst naar beelden die hij nooit gezien heeft: de speler toont niets.
	// Dat trof ook de allereerste kijker, want de camera levert zijn keyframe
	// terwijl die nog aan het aanhaken is.
	gop      [][]byte
	gopBytes int
	// idleSince is wanneer de laatste kijker wegging. Een camera die beeld
	// blijft sturen voor niemand kost bandbreedte in huis en rekentijd op de
	// console, dus na een tijdje zonder kijkers gaat de verbinding dicht.
	idleSince time.Time
}

// viewer is one HTTP response that follows this camera.
//
// A fragmented video may only resume at a keyframe after a fragment was lost.
// needsKeyframe is therefore part of the viewer rather than the camera: one
// slow connection must not disturb viewers that are keeping up.
type viewer struct {
	frames        chan viewerFrame
	generation    atomic.Uint64
	needsKeyframe bool
}

type viewerFrame struct {
	fragment   []byte
	generation uint64
}

// idleGrace is hoe lang een sessie zonder kijkers blijft staan.
//
// Niet meteen dicht: wie een pagina ververst of even wegklikt is binnen een paar
// seconden terug, en dan hoeft de camera niet opnieuw op gang te komen -- dat
// kost een DESCRIBE, een SETUP en het wachten op een keyframe.
const idleGrace = 30 * time.Second

// maxGOPBytes begrenst wat er van de lopende GOP bewaard blijft. Een 4K-keyframe
// is al gauw een halve megabyte; acht MiB draagt een ruime gewone GOP zonder dat
// één bekeken camera een groot deel van de heap van een klein slot vasthoudt.
const maxGOPBytes = 8 << 20

// viewerQueueFrames is only a scheduling cushion. Fragments are shared rather
// than copied, but every queued slice keeps its complete frame alive. At 25 fps
// eight frames already absorb 320 ms of jitter; a viewer further behind must
// catch up instead of retaining seconds of obsolete video.
const viewerQueueFrames = 8

// maxSnapshotBytes matches the controller's public image limit. It is a wire
// bound, not a heap allocation: snapshots pass through an 8 KiB buffer.
const maxSnapshotBytes = 4 << 20

// streamer is er één voor de hele app: alle cameras delen dezelfde luisteraar.
var streamer = &streamHost{sessions: map[string]*session{}}

// start opent de luisteraar, één keer.
func (s *streamHost) start() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String(), nil
	}
	listener, err := streamListen()
	if err != nil {
		return "", fmt.Errorf("kan geen luisteraar openen voor camerabeeld: %w", err)
	}
	s.listener = listener
	server := &http.Server{Handler: http.HandlerFunc(s.serve)}
	go server.Serve(listener)
	return listener.Addr().String(), nil
}

// serve bedient /camera/<id>: het kopdeel, en daarna elk fragment zodra het er
// is. De verbinding blijft open zolang er iemand kijkt.
func (s *streamHost) serve(response http.ResponseWriter, request *http.Request) {
	id := request.URL.Query().Get("id")
	if request.URL.Path == "/snapshot" {
		s.serveSnapshot(response, request, id)
		return
	}
	s.mu.Lock()
	current := s.sessions[id]
	s.mu.Unlock()
	if current == nil {
		http.Error(response, "deze camera wordt niet bekeken", http.StatusNotFound)
		return
	}
	viewer, header, gop := current.join()
	defer current.leave(viewer)
	if len(header) == 0 {
		// Zonder het kopdeel kan een speler niets met de fragmenten die volgen.
		// Dat kan alleen als iemand dit adres rechtstreeks opvraagt voordat de
		// camera op gang is; open() wacht er anders op. De statuscode moet hier
		// nog te zetten zijn, dus dit staat vóór WriteHeader.
		http.Error(response, "deze camera is nog niet op gang", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", current.mediaType())
	response.WriteHeader(http.StatusOK)
	if _, err := response.Write(header); err != nil {
		return
	}
	flusher, _ := response.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	// Eerst het lopende beeld inhalen: het keyframe en alles daarna. Zonder dit
	// begint de speler midden in een GOP en toont hij niets tot het volgende
	// keyframe -- en bij de eerste kijker was dat altijd zo.
	for _, fragment := range gop {
		if _, err := response.Write(fragment); err != nil {
			return
		}
	}
	if flusher != nil && len(gop) > 0 {
		flusher.Flush()
	}
	for {
		select {
		case frame, open := <-viewer.frames:
			if !open {
				return
			}
			// Recovery may drain the queue concurrently with this receive. A
			// generation check keeps a stale dependent frame off the wire even
			// when the receiver won that race.
			if frame.generation != viewer.generation.Load() {
				continue
			}
			if _, err := response.Write(frame.fragment); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-request.Context().Done():
			return
		}
	}
}

// open zet een camera aan de gang en levert het adres waar zijn beeld staat.
func (s *streamHost) open(camera, rtspURL string) (string, string, error) {
	address, err := s.start()
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	current := s.sessions[camera]
	s.mu.Unlock()
	if current == nil {
		current, err = s.begin(camera, rtspURL)
		if err != nil {
			return "", "", err
		}
	}
	// Wachten tot het kopdeel er is: dat komt pas als de camera zijn eerste
	// beeld gestuurd heeft, en zonder dat kopdeel kan een speler niets.
	if err := current.waitForHeader(20 * time.Second); err != nil {
		return "", "", err
	}
	return "http://" + address + "/camera?id=" + camera, current.mediaType(), nil
}

// snapshot levert het adres waar het stilstaande beeld van deze camera staat.
//
// Anders dan de stream vraagt dit geen sessie: elke aanroep haalt één verse
// JPEG bij de console. Wel via deze luisteraar en niet rechtstreeks, want het
// adres bij de console vraagt om de API-sleutel -- en die hoort deze app niet
// uit handen te geven.
func (s *streamHost) snapshot(camera string) (string, error) {
	address, err := s.start()
	if err != nil {
		return "", err
	}
	return "http://" + address + "/snapshot?id=" + camera, nil
}

func (s *streamHost) serveSnapshot(response http.ResponseWriter, request *http.Request, camera string) {
	client, err := instance.api()
	if err != nil {
		http.Error(response, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	snapshot, err := client.OpenSnapshot(ctx, camera, true)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	defer snapshot.Body.Close()
	if snapshot.ContentLength > maxSnapshotBytes {
		http.Error(response, "de momentopname is te groot", http.StatusBadGateway)
		return
	}
	contentType := snapshot.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "image/jpeg"
	}
	response.Header().Set("Content-Type", contentType)
	if snapshot.ContentLength >= 0 {
		response.Header().Set("Content-Length", strconv.FormatInt(snapshot.ContentLength, 10))
	}
	buffer := make([]byte, 8<<10)
	_, _ = io.CopyBuffer(response, io.LimitReader(snapshot.Body, maxSnapshotBytes), buffer)
}

func (s *streamHost) begin(camera, rtspURL string) (*session, error) {
	ctx, cancel := context.WithCancel(context.Background())
	current := &session{viewers: map[*viewer]struct{}{}, stop: cancel}
	s.mu.Lock()
	s.sessions[camera] = current
	s.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.sessions, camera)
			s.mu.Unlock()
			current.close()
		}()
		if err := current.pump(ctx, rtspURL); err != nil {
			instance.stulp.Error("UniFi camerabeeld: " + err.Error())
		}
	}()
	return current, nil
}

// pump leest de camera leeg en zet elk frame door naar wie kijkt.
func (s *session) pump(ctx context.Context, rtspURL string) error {
	stream, err := rtsp.Dial(rtspURL, 20*time.Second)
	if err != nil {
		return err
	}
	defer stream.Close()

	// Bij H.264 staat alles wat een speler moet weten in de beschrijving, dus
	// het kopdeel kan meteen. Bij AV1 niet: daar komt de sequence header in de
	// stroom mee, en dan kan het pas als het eerste beeld binnen is.
	var muxer *rtsp.Muxer
	if stream.Codec() == rtsp.H264 {
		muxer, err = rtsp.NewMuxer(stream.SPS(), stream.PPS())
		if err != nil {
			return err
		}
		s.setHeader(muxer.Header(), muxer.MimeType())
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if stream.Keepalive() != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var h264 rtsp.Assembler
	var av1 rtsp.AV1Assembler
	started := false
	idleCheck := time.NewTicker(5 * time.Second)
	defer idleCheck.Stop()
	go func() {
		for {
			select {
			case <-idleCheck.C:
				if s.idle() {
					// De verbinding sluiten laat ReadPacket hieronder
					// terugkeren; dat is de enige manier om een lezer die op
					// beeld wacht wakker te maken.
					stream.Close()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	for ctx.Err() == nil {
		packet, err := stream.ReadPacket(30 * time.Second)
		if err != nil {
			return err
		}
		var unit [][]byte
		var timestamp uint32
		var complete bool
		if stream.Codec() == rtsp.AV1 {
			unit, timestamp, complete = av1.Push(packet)
		} else {
			unit, timestamp, complete = h264.Push(packet)
		}
		if !complete {
			continue
		}
		if muxer == nil {
			// AV1: wachten tot de sequence header langs is geweest.
			sequence := av1.SequenceHeader()
			if sequence == nil {
				continue
			}
			if muxer, err = rtsp.NewAV1Muxer(sequence); err != nil {
				return err
			}
			s.setHeader(muxer.Header(), muxer.MimeType())
		}
		// Beginnen bij een keyframe. Alles ervoor verwijst naar beelden die
		// niemand gezien heeft, en dat geeft een speler die groene blokken toont
		// tot het volgende keyframe.
		if !started {
			if !muxer.Keyframe(unit) {
				continue
			}
			started = true
		}
		keyframe := muxer.Keyframe(unit)
		fragment, err := muxer.Fragment(unit, timestamp)
		if err != nil {
			return err
		}
		s.broadcast(fragment, keyframe)
	}
	return ctx.Err()
}

func (s *session) setHeader(header []byte, mime string) {
	s.mu.Lock()
	s.header, s.mime = header, mime
	s.mu.Unlock()
}

// mediaType is het volledige type zodra de muxer het weet, en anders het
// algemene. Leeg teruggeven zou de pagina laten raden.
func (s *session) mediaType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mime == "" {
		return "video/mp4"
	}
	return s.mime
}

func (s *session) waitForHeader(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ready := s.header != nil
		s.mu.Unlock()
		if ready {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("de camera stuurde geen beeld binnen %s", timeout)
}

func (s *session) join() (*viewer, []byte, [][]byte) {
	// Kort gebufferd: schedulerjitter houdt de camera niet op, maar een trage
	// kijker kan geen seconden oud beeld in de heap vasthouden.
	viewer := &viewer{frames: make(chan viewerFrame, viewerQueueFrames)}
	s.mu.Lock()
	header := s.header
	// Onder dezelfde lock als broadcast, dus wat hierna uitgezonden wordt komt
	// via het kanaal en niet nog een tweede keer uit deze lijst.
	gop := append([][]byte(nil), s.gop...)
	// Een kopdeel kan er al vóór het eerste camerakeyframe zijn, en een te grote
	// GOP wordt bewust niet bewaard. In beide gevallen mag de nieuwe kijker niet
	// op een afhankelijk beeld instappen.
	viewer.needsKeyframe = len(gop) == 0
	s.viewers[viewer] = struct{}{}
	s.idleSince = time.Time{}
	s.mu.Unlock()
	return viewer, header, gop
}

func (s *session) leave(viewer *viewer) {
	s.mu.Lock()
	delete(s.viewers, viewer)
	if len(s.viewers) == 0 {
		s.idleSince = time.Now()
	}
	s.mu.Unlock()
}

// idle zegt of deze sessie lang genoeg zonder kijkers staat om te stoppen.
func (s *session) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.viewers) == 0 && !s.idleSince.IsZero() && time.Since(s.idleSince) > idleGrace
}

// broadcast stuurt een fragment naar iedereen die kijkt.
//
// Wie niet meekomt slaat een fragment over in plaats van de rest op te houden.
// Een haperend beeld bij één kijker is beter dan een camera die stilstaat omdat
// iemands verbinding traag is.
// broadcast stuurt één fragment naar iedereen die kijkt, en houdt bij waar de
// huidige GOP begon zodat een nieuwe kijker daar kan instappen.
func (s *session) broadcast(fragment []byte, keyframe bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case len(fragment) > maxGOPBytes:
		// Ook een keyframe moet onder de grens vallen. De keyframe-tak eerst
		// zetten zou één uitzonderlijk groot frame alsnog onbeperkt bewaren.
		s.gop, s.gopBytes = nil, 0
	case keyframe:
		s.gop, s.gopBytes = [][]byte{fragment}, len(fragment)
	case s.gop != nil && s.gopBytes+len(fragment) <= maxGOPBytes:
		s.gop, s.gopBytes = append(s.gop, fragment), s.gopBytes+len(fragment)
	default:
		// Een GOP die niet meer past bewaren we niet half: daar kan niemand op
		// instappen. Wie nu binnenkomt wacht op het volgende keyframe.
		s.gop, s.gopBytes = nil, 0
	}

	for viewer := range s.viewers {
		viewer.push(fragment, keyframe)
	}
}

// push adds one encoded frame without ever blocking the camera.
//
// Once the queue overflows, later dependent frames are useless: there is a gap
// before them. Keep the still-contiguous tail already queued, discard everything
// after the gap, and resume on the first keyframe. At that point the old tail is
// drained so the viewer catches up immediately and never receives a P-frame
// whose reference frame was dropped.
func (v *viewer) push(fragment []byte, keyframe bool) {
	if v.needsKeyframe {
		if !keyframe {
			return
		}
		v.restartAt(fragment, v.generation.Load())
		v.needsKeyframe = false
		return
	}
	frame := viewerFrame{fragment: fragment, generation: v.generation.Load()}
	select {
	case v.frames <- frame:
		return
	default:
	}
	if keyframe {
		// This keyframe is already a safe restart point. Prefer it over a queue
		// full of older frames instead of waiting for another complete GOP.
		v.restartAt(fragment, v.generation.Add(1))
		return
	}
	// Frames that were still queued are now from the generation before the
	// gap. The HTTP writer checks this tag after receiving, so it cannot race
	// the drain at the next keyframe and put a stale P-frame on the wire.
	v.generation.Add(1)
	v.needsKeyframe = true
}

func (v *viewer) restartAt(keyframe []byte, generation uint64) {
	for {
		select {
		case <-v.frames:
		default:
			v.frames <- viewerFrame{fragment: keyframe, generation: generation}
			return
		}
	}
}

func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for viewer := range s.viewers {
		close(viewer.frames)
		delete(s.viewers, viewer)
	}
}
