// Package imageshare houdt een afbeelding net lang genoeg vast om hem ergens te
// laten zien waar geen API-sleutel bij kan.
//
// Het geval waarvoor dit bestaat: een pushbericht met de foto van een deurbel
// erbij. Er past 3993 bytes in één versleuteld pushbericht en een momentopname
// van een 4K-camera is een megabyte, dus gaat het adres mee en niet de foto. De
// service worker van de browser haalt hem op terwijl hij de melding toont, en die
// heeft geen sleutel: die ligt in localStorage, waar een service worker niet bij
// kan.
//
// Wat het adres dan beschermt is dat het niet te raden valt en niet lang bestaat:
// 128 bits toeval, een kwartier geldig, en het wijst naar bytes in het geheugen in
// plaats van naar de camera. Bij een pushbericht reist het bovendien versleuteld
// mee; de pushdienst ziet het niet.
//
// In het geheugen en nergens anders, net als de statistiek. Wat er nooit is
// opgeschreven hoeft ook niet opgeruimd te worden.
package imageshare

import (
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
	// MaxBytes begrenst één afbeelding. Bij 4K is een JPEG rond de megabyte; vier
	// keer dat is ruim, en het is er zodat een plugin die iets anders serveert het
	// geheugen niet kan laten vollopen.
	MaxBytes = 4 << 20
	// MaxImages is hoeveel afbeeldingen er samen mogen wachten. Meer dan een paar
	// meldingen tegelijk komt in een huis niet voor, en de oudste gaat eruit.
	MaxImages = 8
)

// Image is wat er bewaard wordt.
type Image struct {
	Data        []byte
	ContentType string
	expires     time.Time
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

// Put bewaart een afbeelding en geeft het id waarmee hij te halen is.
func (s *Store) Put(data []byte, contentType string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("een lege afbeelding valt niet te delen")
	}
	if len(data) > MaxBytes {
		return "", fmt.Errorf("de afbeelding is %d bytes en er mogen er %d in", len(data), MaxBytes)
	}
	if contentType == "" {
		return "", errors.New("een afbeelding zonder type valt niet te tonen")
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
	s.items[id] = Image{Data: data, ContentType: contentType, expires: s.now().Add(Lifetime)}
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
// Buiten /api/ met opzet: de service worker die hem ophaalt heeft geen
// API-sleutel. Zie de uitleg bovenaan dit bestand.
func Path(id string) string { return "/image/" + id }
