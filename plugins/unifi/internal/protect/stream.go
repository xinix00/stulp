package protect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"time"

	"github.com/xinix00/stulp/plugins/unifi/internal/ws"
)

// De twee stromen die de console aanbiedt.
//
// subscribe/devices meldt wat een apparaat is: aan, uit, verbonden, welke stand.
// subscribe/events meldt wat er gebeurt: er is beweging, er wordt aangebeld, er
// is een persoon herkend. Dat zijn twee verschillende soorten bericht en de
// console houdt ze bewust apart, dus deze plugin ook.

// DeviceMessage komt van subscribe/devices.
type DeviceMessage struct {
	Type string          `json:"type"`
	Item json.RawMessage `json:"item"`
}

// EventMessage komt van subscribe/events.
type EventMessage struct {
	Type string `json:"type"` // add of update
	Item struct {
		ID               string   `json:"id"`
		Type             string   `json:"type"` // ring, motion, smartDetectZone
		Device           string   `json:"device"`
		Start            int64    `json:"start"`
		End              int64    `json:"end"`
		SmartDetectTypes []string `json:"smartDetectTypes"`
	} `json:"item"`
}

// Identify leest het model en het id uit een apparaatbericht, zonder het hele
// object te ontleden. Welk model het is bepaalt wie de rest mag lezen.
func (m DeviceMessage) Identify() (modelKey, id string, ok bool) {
	var head struct {
		ModelKey string `json:"modelKey"`
		ID       string `json:"id"`
	}
	if json.Unmarshal(m.Item, &head) != nil || head.ModelKey == "" || head.ID == "" {
		return "", "", false
	}
	return head.ModelKey, head.ID, true
}

// Handlers is wat de plugin met de stroom doet.
type Handlers struct {
	// OnDevice krijgt elk bericht van subscribe/devices.
	OnDevice func(DeviceMessage)
	// OnEvent krijgt elk bericht van subscribe/events.
	OnEvent func(EventMessage)
	// OnConnect draait na elke geslaagde verbinding, ook na een herverbinding.
	// Dat is de plek om de stand opnieuw op te halen: wat er tijdens de
	// onderbreking gebeurd is heeft niemand gezien.
	OnConnect func()
	// OnError meldt wat er misging. Een onderbreking is normaal en hoort niet
	// als storing te lezen; dit is voor wie wil weten waaróm.
	OnError func(error)
}

// pingInterval is hoe vaak we laten weten dat we er nog zijn. De oude app deed
// dertig seconden en dat werkt; korter belast de console zonder iets te winnen.
const pingInterval = 30 * time.Second

// Listen houdt beide stromen open tot ctx eindigt.
//
// Elke stroom heeft zijn eigen verbinding en zijn eigen herstel: valt de ene
// weg, dan blijft de andere lopen. Ze samenvoegen zou betekenen dat een storing
// in de gebeurtenissen ook de standen stilzet.
func (c *Client) Listen(ctx context.Context, handlers Handlers) {
	go c.listen(ctx, "subscribe/devices", handlers, func(raw []byte) {
		if handlers.OnDevice == nil {
			return
		}
		var message DeviceMessage
		if json.Unmarshal(raw, &message) == nil && len(message.Item) > 0 {
			handlers.OnDevice(message)
		}
	})
	go c.listen(ctx, "subscribe/events", handlers, func(raw []byte) {
		if handlers.OnEvent == nil {
			return
		}
		var message EventMessage
		if json.Unmarshal(raw, &message) == nil && message.Item.Device != "" {
			handlers.OnEvent(message)
		}
	})
}

// listen houdt één stroom open, met begrensde backoff.
//
// Bij elke geslaagde verbinding gaat de wachttijd terug naar het begin. Een
// console die één keer per dag even wegvalt hoort niet na een week op een
// wachttijd van een half uur te staan.
func (c *Client) listen(ctx context.Context, resource string, handlers Handlers, deliver func([]byte)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.stream(ctx, resource, handlers, deliver)
		if ctx.Err() != nil {
			return
		}
		if err != nil && handlers.OnError != nil {
			handlers.OnError(err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) stream(ctx context.Context, resource string, handlers Handlers, deliver func([]byte)) error {
	header := http.Header{}
	header.Set("X-API-KEY", c.Token)
	conn, err := ws.Dial("wss://"+c.Address()+c.Path(resource, nil), ws.Options{
		Header: header,
		TLS:    &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	// De verbinding sluiten als de context eindigt: Read blokkeert anders tot
	// de console iets stuurt, en dat kan uren duren.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if conn.Ping() != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	if handlers.OnConnect != nil {
		handlers.OnConnect()
	}
	for {
		message, err := conn.Read()
		if err != nil {
			return err
		}
		deliver(message)
	}
}
