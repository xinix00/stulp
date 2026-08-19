package imageshare

import (
	"context"
	"strings"
	"testing"
	"time"
)

// clock laat een test de tijd verzetten zonder te wachten.
func fixed(store *Store, at *time.Time) {
	store.now = func() time.Time { return *at }
}

func source(url string) Resolver {
	return func(context.Context) (Source, error) {
		return Source{URL: url, ContentType: "image/jpeg"}, nil
	}
}

func TestAnImageResolverComesBack(t *testing.T) {
	store := New()
	id, err := store.Put(source("http://camera/snapshot"))
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
	resolved, err := image.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.URL != "http://camera/snapshot" || resolved.ContentType != "image/jpeg" {
		t.Fatalf("de bron kwam terug als %+v", resolved)
	}
}

func TestTwoImagesGetDifferentAddresses(t *testing.T) {
	store := New()
	first, err := store.Put(source("http://camera/one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(source("http://camera/two"))
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
	id, err := store.Put(source("http://camera/snapshot"))
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
		id, err := store.Put(source("http://camera/snapshot"))
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
	if _, err := store.Put(nil); err == nil {
		t.Fatal("een afbeelding zonder resolver werd aangenomen")
	}
}

func TestForgetInvalidatesOutstandingImages(t *testing.T) {
	store := New()
	id, err := store.Put(source("http://camera/snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	store.Forget()
	if _, ok := store.Get(id); ok {
		t.Fatal("an image URL from before Forget still works")
	}
}
