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

// SeedAttachSecret zet het geheim, maar alleen als het document er nog geen
// heeft. Voor een node waar het geheim uit de jobspec komt (STULP_ATTACH_SECRET):
// zonder volume is het document per boot vers en zou elk uitgedeeld token bij
// een reboot breken — met dit zaad zijn tokens uit de startup-file houdbaar,
// want token = HMAC(geheim, app-id) en beide kanten kennen het zaad. Een
// geheim dat al in het document staat wint: het volume is de waarheid zodra
// hij er is, de env seedt alleen verse starts.
func (s *Store) SeedAttachSecret(ctx context.Context, secret string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if secret == "" {
		return nil
	}
	s.mu.Lock()
	if s.doc.System.AttachSecret != "" {
		s.mu.Unlock()
		return nil
	}
	s.doc.System.AttachSecret = secret
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("seed attach secret: %w", err)
	}
	return nil
}
