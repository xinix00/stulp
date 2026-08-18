package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// De schakelaars van Stulp zelf.
//
// Ze staan in hetzelfde document als de rest, dus ze gaan mee in een backup en
// overleven een herstart. Er is er nu één -- de statistiek -- en dat is met
// opzet een struct en geen vrije map: wat de kern niet kent hoort hier niet in.

// System levert de huidige schakelaars.
func (s *Store) System(_ context.Context) (System, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.doc.System, nil
}

// SetSystem legt ze vast en meldt de wijziging, zodat wie meeluistert zich kan
// aanpassen zonder herstart.
func (s *Store) SetSystem(ctx context.Context, system System) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.doc.System = system
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("set system settings: %w", err)
	}
	s.publish(Event{Manager: "system", Type: "system.update", ID: "system"})
	return nil
}

// AttachSecret levert de sleutel waaruit de tokens van apps op afstand volgen, en
// maakt hem aan als hij er nog niet is.
//
// Aanmaken bij de eerste vraag en niet bij het openen van het document: een Stulp
// waar geen app op afstand bij hoort, hoort dit geheim niet te hebben. Wie ernaar
// vraagt is Stulp zelf als hij een aanmelding narekent, of iemand die een token
// opvraagt om het in een deployment te zetten.
//
// Leegmaken is roteren: het volgende token dat gevraagd wordt is een ander, en
// alles wat het oude gebruikte komt er niet meer in.
func (s *Store) AttachSecret(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	secret := s.doc.System.AttachSecret
	s.mu.RUnlock()
	if secret != "" {
		return secret, nil
	}

	// 32 bytes uit crypto/rand: genoeg dat raden geen strategie is.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("attach secret: %w", err)
	}
	fresh := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	// Nog een keer kijken: twee aanmeldingen tegelijk mogen niet elk hun eigen
	// sleutel maken, want dan is het token van de een ongeldig zodra de ander
	// bewaard heeft.
	if s.doc.System.AttachSecret != "" {
		secret = s.doc.System.AttachSecret
		s.mu.Unlock()
		return secret, nil
	}
	s.doc.System.AttachSecret = fresh
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("attach secret: %w", err)
	}
	return fresh, nil
}
