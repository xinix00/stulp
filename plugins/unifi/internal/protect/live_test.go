package protect

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// Tegen een echte console.
//
// Draait alleen met STULP_UNIFI_HOST en STULP_UNIFI_KEY. Dit is wat een
// nagebouwde console niet kan bewijzen: dat de velden heten zoals wij denken en
// dat de stromen aankomen.
func realClient(t *testing.T) *Client {
	t.Helper()
	host, key := os.Getenv("STULP_UNIFI_HOST"), os.Getenv("STULP_UNIFI_KEY")
	if host == "" || key == "" {
		t.Skip("geen STULP_UNIFI_HOST/KEY; deze toets vraagt een echte console")
	}
	port := 443
	if text := os.Getenv("STULP_UNIFI_PORT"); text != "" {
		port, _ = strconv.Atoi(text)
	}
	return New(host, port, key)
}

// De twee websockets: komen ze open, en houden ze open.
func TestRealConsoleStreams(t *testing.T) {
	client := realClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	devices := make(chan string, 32)
	events := make(chan string, 32)
	connects := make(chan struct{}, 4)
	problems := make(chan error, 8)
	client.Listen(ctx, Handlers{
		OnDevice: func(message DeviceMessage) {
			model, id, ok := message.Identify()
			if ok {
				select {
				case devices <- model + " " + id + " " + message.Type:
				default:
				}
			}
		},
		OnEvent: func(message EventMessage) {
			select {
			case events <- message.Item.Type + " " + message.Type:
			default:
			}
		},
		OnConnect: func() {
			select {
			case connects <- struct{}{}:
			default:
			}
		},
		OnError: func(err error) {
			select {
			case problems <- err:
			default:
			}
		},
	})

	// Beide stromen horen binnen een paar seconden open te zijn.
	opened := 0
	deadline := time.After(15 * time.Second)
	for opened < 2 {
		select {
		case <-connects:
			opened++
		case err := <-problems:
			t.Fatalf("de console weigerde de stroom: %v", err)
		case <-deadline:
			t.Fatalf("maar %d van de 2 stromen open na vijftien seconden", opened)
		}
	}
	t.Logf("  beide websockets open")

	// Wat er in de resterende tijd langskomt. Nul is geen fout -- een stil huis
	// stuurt niets -- maar het is wel het interessante deel van de uitslag.
	time.Sleep(8 * time.Second)
	cancel()
	t.Logf("  %d apparaatberichten, %d gebeurtenissen", len(devices), len(events))
	for len(devices) > 0 {
		t.Logf("    stand: %s", <-devices)
	}
	for len(events) > 0 {
		t.Logf("    gebeurtenis: %s", <-events)
	}
}
