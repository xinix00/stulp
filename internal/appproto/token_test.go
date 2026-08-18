package appproto

import "testing"

// De eigenschap waar het hele schema om gaat: het token van de ene app is niets
// waard voor de andere. Zonder dat is het één gedeeld wachtwoord, en dan kan een
// app die uitlekt zich voordoen als elke andere app in huis.
func TestATokenIsBoundToItsApp(t *testing.T) {
	const secret = "geheim-uit-het-document"
	const nonce = "een-nonce"
	weather := Token(secret, "com.stulp.weather")
	matter := Token(secret, "com.stulp.matter")

	if weather == matter {
		t.Fatal("two apps share one token")
	}
	if !CheckProof(secret, "com.stulp.weather", nonce, AppProof(weather, nonce, "com.stulp.weather")) {
		t.Fatal("an app's own proof was refused")
	}
	// Het bewijs van de weather-app, aangeboden als de matter-app.
	if CheckProof(secret, "com.stulp.matter", nonce, AppProof(weather, nonce, "com.stulp.weather")) {
		t.Fatal("the weather app's proof opened the matter app")
	}
}

// Het antwoord hoort bij één nonce en bij niets anders. Dit is wat het verschil
// maakt met een token opsturen: wie meeleest heeft er de volgende keer niets aan.
func TestAProofIsGoodForOneNonceOnly(t *testing.T) {
	const secret = "geheim"
	const appID = "com.stulp.weather"
	token := Token(secret, appID)

	first, err := Nonce()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Nonce()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two nonces came out the same")
	}
	answer := AppProof(token, first, appID)
	if !CheckProof(secret, appID, first, answer) {
		t.Fatal("the answer to this nonce was refused")
	}
	if CheckProof(secret, appID, second, answer) {
		t.Fatal("an answer replayed against another nonce was accepted")
	}
}

// Beide kanten bewijzen zich. Zonder de tweede helft kan iemand zich als Stulp
// voordoen en een app apparaten en instellingen voorschotelen die niet bestaan.
func TestStulpProvesItselfToTheApp(t *testing.T) {
	const secret = "geheim"
	const appID = "com.stulp.weather"
	token := Token(secret, appID)
	nonce, err := Nonce()
	if err != nil {
		t.Fatal(err)
	}

	if !CheckStulpProof(token, appID, nonce, StulpProof(token, nonce, appID)) {
		t.Fatal("stulp's own proof was refused")
	}
	// Het bewijs van de app, teruggestuurd alsof het van Stulp kwam. De richting
	// zit in de HMAC, dus dit hoort niet te werken.
	if CheckStulpProof(token, appID, nonce, AppProof(token, nonce, appID)) {
		t.Fatal("the app's proof passed as stulp's")
	}
	// Een nep-Stulp die het token niet kent.
	if CheckStulpProof(token, appID, nonce, StulpProof(Token("ander-geheim", appID), nonce, appID)) {
		t.Fatal("a stulp that does not know the token was believed")
	}
}

func TestATokenFollowsFromTheSecret(t *testing.T) {
	const appID = "com.stulp.weather"
	first := Token("een", appID)
	if second := Token("een", appID); first != second {
		t.Fatal("the same secret and app gave two different tokens")
	}
	// Roteren is het geheim weggooien, en dan hoort elk oud bewijs te vervallen.
	if CheckProof("twee", appID, "nonce", AppProof(first, "nonce", appID)) {
		t.Fatal("a proof survived a rotated secret")
	}
}

// Fail closed. Een Stulp die nog geen geheim heeft, hoort geen app op afstand
// binnen te laten in plaats van elke app.
func TestNothingIsAcceptedWithoutASecretNonceOrProof(t *testing.T) {
	const appID = "com.stulp.weather"
	if CheckProof("", appID, "nonce", AppProof(Token("", appID), "nonce", appID)) {
		t.Fatal("an empty secret accepted a proof derived from it")
	}
	if CheckProof("geheim", appID, "", "iets") {
		t.Fatal("a proof without a nonce was accepted")
	}
	if CheckProof("geheim", appID, "nonce", "") {
		t.Fatal("an empty proof was accepted")
	}
}
