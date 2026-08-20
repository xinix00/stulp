package appsdk

import (
	"strings"
	"testing"
)

type pairNameDriver struct{ handlers map[string]PairHandler }

func (d pairNameDriver) NewDevice(*Device) (DeviceHandler, error) { return nil, nil }
func (d pairNameDriver) Pair() map[string]PairHandler             { return d.handlers }

// Een koppelbericht wordt één padsegment in de emit-URL
// (/api/stulp/pair/{id}/emit/{event}). Een naam met een '/' erin komt daar als
// %2F aan en leanhttp weigert die dubbelzinnigheid met een 400 — bij de
// gebruiker, zonder aanwijzing (gemeten 20-08). Dus weigeren we hem waar het
// nog uit te leggen is: bij het openen van de sessie, met de naam erbij.
func TestPairEventNameMustSurviveTheEmitURL(t *testing.T) {
	open := func(name string) ([]string, error) {
		p := &process{plugin: Plugin{Drivers: map[string]Driver{
			"thing": pairNameDriver{handlers: map[string]PairHandler{
				name: func(any) (any, error) { return nil, nil },
			}},
		}}}
		return p.startPair("thing", "session-1")
	}

	for _, bad := range []string{"commission/state", "a?b", "a#b", "a%2Fb", ""} {
		names, err := open(bad)
		if err == nil {
			t.Fatalf("naam %q hoort geweigerd te worden, kreeg %v", bad, names)
		}
		if bad != "" && !strings.Contains(err.Error(), bad) {
			t.Fatalf("de fout noemt %q niet: %v", bad, err)
		}
	}
	if _, err := open("commission_state"); err != nil {
		t.Fatalf("een gewone naam hoort te mogen: %v", err)
	}
}
