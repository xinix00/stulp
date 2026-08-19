// Package imageshare houdt een afbeeldingsbron net lang genoeg vast om hem in
// een pushmelding te laten zien zonder dat er een open browsersessie nodig is.
//
// Het geval waarvoor dit bestaat: een pushbericht met de foto van een deurbel
// erbij. Er past 3993 bytes in één versleuteld pushbericht en een momentopname
// van een 4K-camera is een megabyte, dus gaat het adres mee en niet de foto. De
// service worker van de browser haalt hem op terwijl hij de melding toont, ook
// wanneer Manage niet open staat.
//
// Wat het adres dan beschermt is dat het niet te raden valt en niet lang bestaat:
// 128 bits toeval en een kwartier geldig. Het wijst naar een resolver die pas bij
// het ophalen een verse bron aan de camera-app vraagt; de foto zelf staat dus
// nooit in dit register. Bij een pushbericht reist het adres bovendien
// versleuteld mee; de pushdienst ziet het niet.
//
// In het geheugen en nergens anders, net als de statistiek. Wat er nooit is
// opgeschreven hoeft ook niet opgeruimd te worden.
package imageshare

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// Lifetime is hoe lang een afbeelding opvraagbaar blijft. Ruimer dan de
	// bezorging van een pushbericht, want een telefoon toont de melding pas als
	// hij hem krijgt en haalt de foto dan op.
	Lifetime = 15 * time.Minute
	// MaxBytes begrenst hoeveel bytes de weblaag uit één bron doorgeeft. Bij 4K is
	// een JPEG rond de megabyte; vier keer dat is ruim. De bytes worden gestreamd
	// en staan niet in Store.
	MaxBytes = 4 << 20
	// MaxImages is hoeveel tijdelijke adressen er samen mogen wachten. Meer dan een
	// paar meldingen tegelijk komt in een huis niet voor, en de oudste gaat eruit.
	MaxImages = 8
)

// Source is het kortlevende adres dat een app voor één afbeelding aanbiedt.
type Source struct {
	URL         string
	ContentType string
}

// Resolver vraagt de app pas bij het HTTP-verzoek om een verse bron. Zo kost
// ImageURL geen camera-aanroep en houdt geen enkel Stulp-proces beeldbytes vast.
type Resolver func(context.Context) (Source, error)

// Image is wat er bewaard wordt: een kleine functie, nooit de afbeelding zelf.
type Image struct {
	Resolve Resolver
	expires time.Time
}

type Store struct {
	mu     sync.Mutex
	items  map[string]Image
	now    func() time.Time
	random func([]byte) (int, error)
}

func New() *Store {
	return &Store{items: map[string]Image{}, now: time.Now, random: rand.Read}
}

// Put bewaart een resolver en geeft het id waarmee zijn afbeelding te halen is.
func (s *Store) Put(resolve Resolver) (string, error) {
	if resolve == nil {
		return "", errors.New("een afbeelding zonder bron valt niet te delen")
	}
	var value [16]byte
	if _, err := s.random(value[:]); err != nil {
		return "", fmt.Errorf("id maken: %w", err)
	}
	id := fmt.Sprintf("%x", value[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	// Vol: de oudste eruit. Een nieuwe melding is actueler dan een oude die nog
	// niemand opgehaald heeft.
	for len(s.items) >= MaxImages {
		oldest, found := "", time.Time{}
		for key, item := range s.items {
			if found.IsZero() || item.expires.Before(found) {
				oldest, found = key, item.expires
			}
		}
		if oldest == "" {
			break
		}
		delete(s.items, oldest)
	}
	s.items[id] = Image{Resolve: resolve, expires: s.now().Add(Lifetime)}
	return id, nil
}

func (s *Store) Get(id string) (Image, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	item, ok := s.items[id]
	return item, ok
}

// Forget invalidates every outstanding image URL. A restore is a boundary
// between two house configurations; a temporary URL from the previous one must
// not keep serving an old camera image afterwards.
func (s *Store) Forget() {
	s.mu.Lock()
	s.items = map[string]Image{}
	s.mu.Unlock()
}

func (s *Store) evictLocked() {
	now := s.now()
	for key, item := range s.items {
		if item.expires.Before(now) {
			delete(s.items, key)
		}
	}
}

// Path is waar een gedeelde afbeelding te halen valt.
//
// Buiten /api/ met opzet: de service worker moet hem zonder open browsersessie
// kunnen ophalen. Zie de uitleg bovenaan dit bestand.
func Path(id string) string { return "/image/" + id }
