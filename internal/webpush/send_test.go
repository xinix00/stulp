package webpush_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/webpush"
	"github.com/xinix00/stulp/internal/webpush/webpushtest"
)

func TestSendDeliversAMessageTheBrowserCanRead(t *testing.T) {
	service := webpushtest.New(t)
	sender := webpush.Sender{Client: service.Client(), Subject: "mailto:iemand@example.net"}
	key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), key, service.Subscription(),
		webpush.Message{Title: "Stulp", Body: "Iemand belt aan", URL: "/"}, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	messages := service.Messages()
	if len(messages) != 1 {
		t.Fatalf("de telefoon kreeg %d berichten", len(messages))
	}
	if messages[0].Title != "Stulp" || messages[0].Body != "Iemand belt aan" || messages[0].URL != "/" {
		t.Fatalf("de telefoon zou %+v tonen", messages[0])
	}
	headers := service.Headers()
	for header, want := range map[string]string{
		"Content-Encoding": "aes128gcm",
		"Content-Type":     "application/octet-stream",
		"Ttl":              "90",
		"Urgency":          "high",
	} {
		if got := headers[0].Get(header); got != want {
			t.Fatalf("%s is %q en moet %q zijn", header, got, want)
		}
	}
	if !strings.HasPrefix(headers[0].Get("Authorization"), "vapid t=") {
		t.Fatalf("Authorization is %q", headers[0].Get("Authorization"))
	}
}

// Een bericht dat niet in één push past wordt geweigerd en niet afgekapt. Een
// melding die halverwege ophoudt is erger dan een Flow die zegt dat het niet ging.
func TestSendRefusesAMessageThatDoesNotFit(t *testing.T) {
	service := webpushtest.New(t)
	sender := webpush.Sender{Client: service.Client()}
	key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), key, service.Subscription(),
		webpush.Message{Title: "Stulp", Body: strings.Repeat("a", webpush.MaxPayload)}, time.Minute)
	if err == nil {
		t.Fatal("een te lang bericht werd aangenomen")
	}
	if !strings.Contains(err.Error(), "passen") {
		t.Fatalf("de fout legt niet uit dat het bericht te lang is: %v", err)
	}
	if len(service.Messages()) != 0 {
		t.Fatal("er is alsnog iets verstuurd")
	}
}

func TestSendReportsAnAbandonedSubscriptionAsGone(t *testing.T) {
	key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		service := webpushtest.New(t)
		service.Answer(status, "")
		sender := webpush.Sender{Client: service.Client()}
		err := sender.Send(context.Background(), key, service.Subscription(),
			webpush.Message{Body: "hoi"}, time.Minute)
		if !errors.Is(err, webpush.ErrGone) {
			t.Fatalf("status %d gaf %v en niet ErrGone", status, err)
		}
	}
}

// Waarom een pushdienst weigert staat in zijn antwoord. Een 403 op een verkeerde
// VAPID-sleutel is iets anders dan een 429 omdat het te snel gaat, en dat
// verschil hoort in de melding te staan die iemand te zien krijgt.
func TestSendRepeatsWhyThePushServiceRefused(t *testing.T) {
	service := webpushtest.New(t)
	service.Answer(http.StatusForbidden, "VAPID credentials do not match")
	sender := webpush.Sender{Client: service.Client()}
	key, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), key, service.Subscription(),
		webpush.Message{Body: "hoi"}, time.Minute)
	if err == nil {
		t.Fatal("een geweigerd bericht gold als bezorgd")
	}
	if errors.Is(err, webpush.ErrGone) {
		t.Fatal("een 403 werd als een verdwenen abonnement gelezen")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "VAPID credentials do not match") {
		t.Fatalf("de fout zegt niet wat de pushdienst zei: %v", err)
	}
}
