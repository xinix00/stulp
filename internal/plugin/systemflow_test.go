package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xinix00/stulp/internal/store"
)

func TestSystemFlowTriggerIsPublishedUnderStulp(t *testing.T) {
	database, err := store.Open(store.InMemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	events, cancel := database.Subscribe(1)
	defer cancel()

	process := &Process{store: database, app: store.App{ID: "com.example.plugin"}}
	params, err := json.Marshal(map[string]any{
		"kind": "trigger", "id": "capability.button.on", "system": true,
		"tokens": map[string]any{"value": true}, "state": map[string]any{"deviceId": "button-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.handle(context.Background(), "flow.trigger", params); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		data, _ := event.Data.(map[string]any)
		if event.Manager != "flow" || event.Type != "card.trigger" || event.ID != "capability.button.on" ||
			data["appId"] != "stulp" {
			t.Fatalf("system trigger event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("system flow trigger was not published")
	}
}
