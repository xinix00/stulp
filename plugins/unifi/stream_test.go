package main

import (
	"fmt"
	"slices"
	"testing"
)

func TestSlowViewerRestartsAtNextKeyframe(t *testing.T) {
	t.Run("after a dependent frame was lost", func(t *testing.T) {
		session := startedTestSession()
		slow, _, _ := session.join()

		fillViewer(session, "old")
		session.broadcast([]byte("lost"), false)
		// Even when the writer makes room again, a dependent frame after the
		// gap may not enter the queue.
		_ = queuedFrames(slow)
		session.broadcast([]byte("also-lost"), false)
		if len(slow.frames) != 0 {
			t.Fatal("dependent frame entered the queue after a gap")
		}

		session.broadcast([]byte("new-key"), true)
		session.broadcast([]byte("new-delta"), false)
		if got, want := queuedFrames(slow), []string{"new-key", "new-delta"}; !slices.Equal(got, want) {
			t.Fatalf("frames after overflow = %v, want %v", got, want)
		}

		// Resynchronizing one viewer must not disturb the GOP retained for a
		// viewer that joins afterwards.
		_, _, gop := session.join()
		if got, want := stringsOf(gop), []string{"new-key", "new-delta"}; !slices.Equal(got, want) {
			t.Fatalf("retained GOP = %v, want %v", got, want)
		}
	})

	t.Run("when the keyframe itself meets a full queue", func(t *testing.T) {
		session := startedTestSession()
		slow, _, _ := session.join()

		fillViewer(session, "old")
		session.broadcast([]byte("new-key"), true)
		session.broadcast([]byte("new-delta"), false)
		if got, want := queuedFrames(slow), []string{"new-key", "new-delta"}; !slices.Equal(got, want) {
			t.Fatalf("frames after full-queue keyframe = %v, want %v", got, want)
		}
	})
}

func TestSlowViewerDoesNotDisturbFastViewer(t *testing.T) {
	session := startedTestSession()
	slow, _, _ := session.join()
	fast, _, _ := session.join()

	for i := range viewerQueueFrames {
		frame := []byte(fmt.Sprintf("delta-%d", i))
		session.broadcast(frame, false)
		if got := string((<-fast.frames).fragment); got != string(frame) {
			t.Fatalf("fast viewer received %q, want %q", got, frame)
		}
	}
	if len(slow.frames) != viewerQueueFrames {
		t.Fatalf("slow queue contains %d frames, want %d", len(slow.frames), viewerQueueFrames)
	}

	session.broadcast([]byte("overflow"), false)
	if got := string((<-fast.frames).fragment); got != "overflow" {
		t.Fatalf("fast viewer lost the overflow frame: got %q", got)
	}
	session.broadcast([]byte("new-key"), true)
	if got := string((<-fast.frames).fragment); got != "new-key" {
		t.Fatalf("fast viewer lost the recovery keyframe: got %q", got)
	}
	session.broadcast([]byte("new-delta"), false)
	if got := string((<-fast.frames).fragment); got != "new-delta" {
		t.Fatalf("fast viewer lost the frame after recovery: got %q", got)
	}
	if got, want := queuedFrames(slow), []string{"new-key", "new-delta"}; !slices.Equal(got, want) {
		t.Fatalf("slow viewer recovered with %v, want %v", got, want)
	}
}

func TestOverflowInvalidatesAFrameTakenDuringRecovery(t *testing.T) {
	session := startedTestSession()
	viewer, _, _ := session.join()
	fillViewer(session, "old")

	session.broadcast([]byte("lost"), false)
	stale := <-viewer.frames
	if stale.generation == viewer.generation.Load() {
		t.Fatal("a queued frame from before the gap still belongs to the current generation")
	}

	session.broadcast([]byte("new-key"), true)
	current := <-viewer.frames
	if current.generation != viewer.generation.Load() || string(current.fragment) != "new-key" {
		t.Fatalf("recovery frame = %q generation %d, current generation %d", current.fragment, current.generation, viewer.generation.Load())
	}
}

func TestViewerWithoutRetainedGOPWaitsForKeyframe(t *testing.T) {
	session := &session{viewers: map[*viewer]struct{}{}}
	viewer, _, gop := session.join()
	if len(gop) != 0 {
		t.Fatalf("new viewer received a GOP before the first keyframe: %v", stringsOf(gop))
	}

	session.broadcast([]byte("dependent"), false)
	if len(viewer.frames) != 0 {
		t.Fatal("new viewer started on a dependent frame")
	}
	session.broadcast([]byte("first-key"), true)
	if got, want := queuedFrames(viewer), []string{"first-key"}; !slices.Equal(got, want) {
		t.Fatalf("first decodable frames = %v, want %v", got, want)
	}
}

func startedTestSession() *session {
	session := &session{viewers: map[*viewer]struct{}{}}
	session.broadcast([]byte("initial-key"), true)
	return session
}

func fillViewer(session *session, prefix string) {
	for i := range viewerQueueFrames {
		session.broadcast([]byte(fmt.Sprintf("%s-%d", prefix, i)), false)
	}
}

func queuedFrames(viewer *viewer) []string {
	frames := make([]string, 0, len(viewer.frames))
	for len(viewer.frames) > 0 {
		frames = append(frames, string((<-viewer.frames).fragment))
	}
	return frames
}

func stringsOf(frames [][]byte) []string {
	text := make([]string, len(frames))
	for i, frame := range frames {
		text[i] = string(frame)
	}
	return text
}
