package webapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/stulphttp"
)

func TestSharedImageIsResolvedAndStreamedOnDemand(t *testing.T) {
	picture := []byte{0x89, 'P', 'N', 'G', 's', 'm', 'a', 'l', 'l'}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "image/png")
		response.Write(picture)
	}))
	defer upstream.Close()

	store := imageshare.New()
	resolved := 0
	id, err := store.Put(func(context.Context) (imageshare.Source, error) {
		resolved++
		return imageshare.Source{URL: upstream.URL}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 0 {
		t.Fatal("creating the notification URL already requested the picture")
	}

	server := &Server{mux: stulphttp.NewServeMux(), images: store}
	server.handleImages()
	request := httptest.NewRequest(http.MethodGet, imageshare.Path(id), nil)
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, request)

	if resolved != 1 {
		t.Fatalf("the browser request resolved the picture %d times", resolved)
	}
	if response.Code != http.StatusOK || response.Body.String() != string(picture) {
		t.Fatalf("image response = %d %x", response.Code, response.Body.Bytes())
	}
	if response.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestSharedImageRejectsADeclaredOversizeBeforeReading(t *testing.T) {
	var requested atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "image/jpeg")
		response.Header().Set("Content-Length", strconv.Itoa(imageshare.MaxBytes+1))
		response.WriteHeader(http.StatusOK)
		requested.Store(true)
	}))
	defer upstream.Close()

	store := imageshare.New()
	id, err := store.Put(func(context.Context) (imageshare.Source, error) {
		return imageshare.Source{URL: upstream.URL, ContentType: "image/jpeg"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{mux: stulphttp.NewServeMux(), images: store}
	server.handleImages()
	request := httptest.NewRequest(http.MethodGet, imageshare.Path(id), nil)
	response := httptest.NewRecorder()
	server.mux.ServeHTTP(response, request)

	if !requested.Load() {
		t.Fatal("the source was not requested")
	}
	if response.Code != http.StatusBadGateway {
		t.Fatalf("oversize image returned %d: %s", response.Code, response.Body.String())
	}
}
