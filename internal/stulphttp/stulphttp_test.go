package stulphttp

import (
	"io"
	"net/http/httptest"
	"testing"
)

// TestVormIsDeDoorsnede: de vier plekken waar net/http en leanhttp uiteenlopen
// gaan door deze functies, en dit is de test die vastlegt dat ze op een host
// doen wat net/http deed. De node-kant heeft dezelfde namen; wat daar getoetst
// wordt staat in leanhttp zelf (mux_test.go, serve_test.go).
func TestVormIsDeDoorsnede(t *testing.T) {
	mux := NewServeMux()
	mux.HandleFunc("GET /api/devices/{id}", func(w ResponseWriter, r *Request) {
		if got := r.PathValue("id"); got != "lamp-1" {
			t.Errorf("PathValue = %q", got)
		}
		if got := Query(r).Get("since"); got != "10" {
			t.Errorf("Query = %q", got)
		}
		if got := Path(r); got != "/api/devices/lamp-1" {
			t.Errorf("Path = %q", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(StatusOK)
		w.Write([]byte("ok"))
		if !Flush(w) {
			t.Error("Flush zei nee op een writer die het kan")
		}
	})

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/devices/lamp-1?since=10", nil))

	if recorder.Code != StatusOK {
		t.Errorf("status = %d", recorder.Code)
	}
	body, _ := io.ReadAll(recorder.Result().Body)
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
}

// TestFoutpaden: Error en NotFound zeggen wat ze moeten zeggen.
func TestFoutpaden(t *testing.T) {
	recorder := httptest.NewRecorder()
	Error(recorder, "kapot", StatusBadGateway)
	if recorder.Code != StatusBadGateway {
		t.Errorf("Error gaf %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	NotFound(recorder, httptest.NewRequest("GET", "/weg", nil))
	if recorder.Code != StatusNotFound {
		t.Errorf("NotFound gaf %d", recorder.Code)
	}
}
