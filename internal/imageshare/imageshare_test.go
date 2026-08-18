package imageshare

import (
	"strings"
	"testing"
	"time"
)

// clock laat een test de tijd verzetten zonder te wachten.
func fixed(store *Store, at *time.Time) {
	store.now = func() time.Time { return *at }
}

func TestAnImageComesBackUnchanged(t *testing.T) {
	store := New()
	id, err := store.Put([]byte("jpeg-bytes"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(Path(id), "/image/") {
		t.Fatalf("het pad is %q en hoort buiten /api/ te liggen", Path(id))
	}
	// 16 bytes toeval in hex: dat is waar het adres zijn bescherming aan heeft.
	if len(id) != 32 {
		t.Fatalf("het id is %d tekens en 128 bits in hex zijn er 32", len(id))
	}
	image, ok := store.Get(id)
	if !ok {
		t.Fatal("de afbeelding was er niet")
	}
	if string(image.Data) != "jpeg-bytes" || image.ContentType != "image/jpeg" {
		t.Fatalf("er kwam %q van type %q uit", image.Data, image.ContentType)
	}
}

func TestTwoImagesGetDifferentAddresses(t *testing.T) {
	store := New()
	first, err := store.Put([]byte("een"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put([]byte("twee"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("twee afbeeldingen kregen hetzelfde adres")
	}
}

// Na de looptijd is de afbeelding weg. Dat is de halve bescherming van een adres
// dat geen sleutel vraagt.
func TestAnImageDisappearsWhenItsTimeIsUp(t *testing.T) {
	store := New()
	now := time.Now()
	fixed(store, &now)
	id, err := store.Put([]byte("jpeg"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(Lifetime - time.Second)
	if _, ok := store.Get(id); !ok {
		t.Fatal("de afbeelding was al weg vóór zijn tijd")
	}
	now = now.Add(2 * time.Second)
	if _, ok := store.Get(id); ok {
		t.Fatal("de afbeelding is er nog na zijn looptijd")
	}
}

// Vol is vol: de oudste gaat eruit. Een nieuwe melding is actueler dan een oude
// die nog niemand opgehaald heeft.
func TestTheOldestImageMakesRoom(t *testing.T) {
	store := New()
	now := time.Now()
	fixed(store, &now)
	ids := make([]string, 0, MaxImages+1)
	for range MaxImages + 1 {
		id, err := store.Put([]byte("jpeg"), "image/jpeg")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		// Elke volgende een seconde later, zodat "oudste" iets betekent.
		now = now.Add(time.Second)
	}
	if _, ok := store.Get(ids[0]); ok {
		t.Fatal("de oudste afbeelding staat er nog terwijl de opslag vol was")
	}
	if _, ok := store.Get(ids[len(ids)-1]); !ok {
		t.Fatal("de nieuwste afbeelding is niet bewaard")
	}
	if len(store.items) != MaxImages {
		t.Fatalf("er staan %d afbeeldingen in plaats van %d", len(store.items), MaxImages)
	}
}

func TestPutRefusesWhatCannotBeShown(t *testing.T) {
	store := New()
	cases := map[string]struct {
		data        []byte
		contentType string
	}{
		"leeg":        {nil, "image/jpeg"},
		"zonder type": {[]byte("jpeg"), ""},
		"te groot":    {make([]byte, MaxBytes+1), "image/jpeg"},
	}
	for name, item := range cases {
		if _, err := store.Put(item.data, item.contentType); err == nil {
			t.Fatalf("%s werd aangenomen", name)
		}
	}
}

func TestForgetInvalidatesOutstandingImages(t *testing.T) {
	store := New()
	id, err := store.Put([]byte("jpeg"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	store.Forget()
	if _, ok := store.Get(id); ok {
		t.Fatal("an image URL from before Forget still works")
	}
}
