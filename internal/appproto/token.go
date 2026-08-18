package appproto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// Hoe een app op afstand zich bewijst.
//
// Over een unix-socket is er niets te bewijzen: de kernel zegt welke uid er aan de
// andere kant zit en daar valt niet over te liegen. Over een poort bestaat die
// vraag wel, en dan is er een geheim nodig.
//
// Eén sleutel voor alle apps, en het token van een app is de HMAC daarvan over
// zijn id. Dat heeft twee eigenschappen die er hier op aankomen:
//
//   - Het token is gebonden aan de app-id. Wie het token van com.stulp.weather
//     heeft, kan zich niet voordoen als com.stulp.matter -- en dat is precies het
//     gat dat één gedeeld wachtwoord voor alle apps zou laten.
//   - Stulp bewaart niets per app. Er is één geheim in het document, elk token is
//     eruit te herleiden wanneer iemand het vraagt, en roteren is dat ene veld
//     leegmaken.
//
// **Het token zelf gaat niet over de lijn.** Beide kanten kennen het, dus is het
// genoeg om te bewijzen dát je het kent: de ander stuurt een nummer dat hij nog
// nooit gestuurd heeft, en je stuurt de HMAC daarvan terug. Wie meeleest ziet een
// antwoord dat bij dat ene nummer hoort en bij niets anders, en heeft daar de
// volgende keer niets aan.
//
// Dat gaat twee kanten op. Stulp bewijst zich net zo goed aan de app, want anders
// kan iemand zich als Stulp voordoen en een app apparaten en instellingen
// voorschotelen die niet bestaan.
//
// Wat dit niet is: geheimhouding. Wie meeleest kan niet inbreken, maar ziet wel
// alles wat er daarna over de lijn gaat -- apparaatnamen, waarden, en de sleutels
// die een app met setting.set bewaart. Daar is TLS voor, en dat is een aparte
// keuze die deze niet vervangt.

// Token levert het token van één app.
func Token(secret, appID string) string {
	return mac([]byte(secret), "token", appID)
}

// De twee richtingen waarin een bewijs kan gaan. Ze staan in de HMAC zodat het
// bewijs van de app niet als het bewijs van Stulp te hergebruiken is.
const (
	fromApp   = "app"
	fromStulp = "stulp"
)

// Proof is het antwoord op één nonce: de HMAC van het token over de richting, de
// nonce en de app-id.
//
// De richting en de app-id zitten erin zodat een bewijs alleen geldig is waar het
// voor bedoeld was. De velden zijn met een nulbyte gescheiden, want anders zouden
// twee andere velden dezelfde reeks bytes kunnen opleveren.
func Proof(token, direction, nonce, appID string) string {
	return mac([]byte(token), direction, nonce, appID)
}

// CheckProof zegt of dit bewijs bij deze nonce en deze app hoort.
//
// Een leeg geheim of een leeg bewijs keurt niets goed: een Stulp die nog geen
// geheim heeft, hoort geen app op afstand binnen te laten in plaats van elke app.
//
// De vergelijking duurt even lang voor een bewijs dat er bijna op lijkt als voor
// een dat er niets op lijkt, zodat er niets uit de tijd te leren valt.
func CheckProof(secret, appID, nonce, offered string) bool {
	if secret == "" || nonce == "" || offered == "" {
		return false
	}
	want := Proof(Token(secret, appID), fromApp, nonce, appID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(offered)) == 1
}

// AppProof is wat een app stuurt om zich te bewijzen.
func AppProof(token, nonce, appID string) string {
	return Proof(token, fromApp, nonce, appID)
}

// StulpProof is wat Stulp terugstuurt om zich aan de app te bewijzen.
func StulpProof(token, nonce, appID string) string {
	return Proof(token, fromStulp, nonce, appID)
}

// CheckStulpProof laat een app narekenen dat hij met de echte Stulp praat.
func CheckStulpProof(token, appID, nonce, offered string) bool {
	if token == "" || nonce == "" || offered == "" {
		return false
	}
	want := StulpProof(token, nonce, appID)
	return subtle.ConstantTimeCompare([]byte(want), []byte(offered)) == 1
}

// Nonce levert een nummer dat nog nooit langsgekomen is.
//
// 32 bytes uit crypto/rand: dat een nonce zich herhaalt is het enige waar dit
// schema niet tegen kan, en bij deze lengte gebeurt dat niet.
func Nonce() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// mac is de HMAC van een reeks velden, gescheiden zodat ze niet in elkaar over
// kunnen lopen.
func mac(key []byte, fields ...string) string {
	hash := hmac.New(sha256.New, key)
	for _, field := range fields {
		hash.Write([]byte(field))
		hash.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}
