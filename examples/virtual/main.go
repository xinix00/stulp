// Command com.stulp.virtual is de test-app: een plugin zonder hardware eronder.
//
// Hij bestaat om de weg van Stulp naar een app-proces te beproeven -- starten,
// koppelen, een capability zetten, een Flow-kaart draaien -- en is meteen het
// kortste voorbeeld van hoe een plugin eruitziet. Zie docs/plugins.md.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/xinix00/lean/leanhttp"

	"github.com/xinix00/stulp/internal/appsdk"
)

// snapshots is het luisterende deel van een cameraplugin, zo klein als het kan.
//
// Een echte plugin haalt hier RTSP op, pakt het om naar iets wat een browser
// aanneemt en bedient dat op zijn eigen luisteraar; Stulp haalt daar op en geeft
// de bytes door. Dit voorbeeld doet hetzelfde met één vierkantje, want het punt
// is de weg en niet het beeld.
//
// Het adres stond hier eerder als http://127.0.0.1:9/snapshot.jpg. Dat is de
// discard-poort: het voorbeeld beloofde beeld dat nooit kon komen. Een voorbeeld
// dat niet werkt leert het verkeerde.
type snapshotHost struct {
	mu       sync.Mutex
	listener net.Listener
	image    []byte
}

var snapshots = &snapshotHost{}

// start opent de luisteraar één keer en geeft zijn adres.
func (h *snapshotHost) start() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listener != nil {
		return "http://" + h.listener.Addr().String(), nil
	}
	drawing := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := range 16 {
		for y := range 16 {
			drawing.Set(x, y, color.RGBA{R: 255, G: 39, B: 29, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, drawing); err != nil {
		return "", err
	}
	h.image = encoded.Bytes()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("kan geen luisteraar openen voor het beeld: %w", err)
	}
	h.listener = listener
	// leanhttp en niet net/http: dit serveert één PNG, en net/http linkt
	// crypto/tls onvoorwaardelijk mee. Op een HopOS-slot is dat ~1 MB image voor
	// een handler van vier regels — gemeten, zie de hopos-targets in build.sh.
	go leanhttp.Serve(listener, func(w leanhttp.ResponseWriter, _ *leanhttp.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(h.image)))
		_, _ = w.Write(h.image)
	})
	return "http://" + listener.Addr().String(), nil
}

type switchDriver struct{}

func (switchDriver) NewDevice(device *appsdk.Device) (appsdk.DeviceHandler, error) {
	return &virtualSwitch{device: device}, nil
}

// Pair is wat de koppelpagina van deze driver mag sturen.
func (switchDriver) Pair() map[string]appsdk.PairHandler {
	return map[string]appsdk.PairHandler{
		"validate": func(any) (any, error) { return "ok", nil },
	}
}

func (switchDriver) ListDevices() ([]appsdk.PairedDevice, error) {
	return []appsdk.PairedDevice{{
		Name:         "Stulp switch",
		Data:         map[string]any{"id": "virtual-1"},
		Capabilities: []string{"onoff"},
	}}, nil
}

type virtualSwitch struct{ device *appsdk.Device }

func (s *virtualSwitch) OnInit() error {
	// Bijhouden hoe vaak dit apparaat gestart is: dat bewijst dat de store
	// bewaart wat de app erin zet, ook over een herstart heen.
	boots, _ := s.device.StoreValue("boots")
	count, _ := boots.(float64)
	// Twee beelden: een stilstaand en een stream. Het adres blijft hier en gaat
	// pas naar Stulp als iemand echt kijkt. Een echte cameraplugin haalt hier
	// RTSP op, pakt het om naar iets wat een browser aanneemt, en bedient dat op
	// zijn eigen luisteraar; Stulp geeft de bytes alleen door.
	if err := s.device.SetCameraImage("live", "Snapshot", func() (appsdk.ImageSource, error) {
		address, err := snapshots.start()
		if err != nil {
			return appsdk.ImageSource{}, err
		}
		return appsdk.ImageSource{URL: address + "/snapshot.png", ContentType: "image/png"}, nil
	}); err != nil {
		return err
	}
	if err := s.device.SetCameraVideo("live", "Live video", func() (appsdk.VideoStream, error) {
		return appsdk.VideoStream{
			URL: "http://127.0.0.1:9/live.mp4", ContentType: "video/mp4",
		}, nil
	}); err != nil {
		return err
	}
	return s.device.SetStore(map[string]any{"boots": count + 1})
}

// OnCapability is wat er gebeurt als iemand de schakelaar omzet. Een echt
// apparaat zou hier het commando versturen; deze bevestigt meteen.
func (s *virtualSwitch) OnCapability(name string, value any) error {
	return s.device.SetCapabilityValue(name, value)
}

func main() { start(plugin()) }

// plugin is de app zelf, los van HOE hij gestart wordt: op een host krijgt hij
// fd 3 van Stulp mee, op een HopOS-node meldt hij zich over een poort. Zie
// start_host.go en start_tamago.go.
func plugin() appsdk.Plugin {
	return appsdk.Plugin{
		OnInit: func(h *appsdk.Stulp) error {
			boots, _ := h.Setting("boots")
			count, _ := boots.(float64)
			if err := h.SetSetting("boots", count+1); err != nil {
				return err
			}

			h.OnFlowAction("ping", func(args, _ map[string]any) (any, error) {
				return fmt.Sprintf("pong:%v", args["value"]), nil
			})
			// Het device-argument komt als id binnen; de naam staat in de
			// lokale kopie, dus daar is geen vraag aan Stulp voor nodig.
			h.OnFlowAction("device_name", func(args, _ map[string]any) (any, error) {
				return h.DeviceName(appsdk.DeviceArg(args, "device")), nil
			})
			h.OnFlowAction("choose", func(args, _ map[string]any) (any, error) {
				return args["choice"], nil
			})
			h.OnFlowAutocomplete("action", "choose", "choice",
				func(query string, _ map[string]any) ([]appsdk.AutocompleteItem, error) {
					return []appsdk.AutocompleteItem{{ID: "one", Name: "One " + query}}, nil
				})
			// De trigger filtert: dezelfde kaart vuurt voor elke Flow, en het
			// argument bepaalt of déze gebeurtenis bij déze Flow hoort.
			h.OnFlowTrigger("signal", func(args, state map[string]any) (bool, error) {
				want, given := args["match"]
				return !given || want == nil || want == state["match"], nil
			})
			// Bijhouden wat er als laatste veranderde, zodat de test kan zien
			// dat een draaiende app het meekrijgt.
			var lastSetting atomic.Value
			h.OnSettingsChanged(func(changed map[string]any) {
				for key := range changed {
					lastSetting.Store(key)
				}
			})

			// Wat de settings-pagina van deze app kan aanroepen.
			h.OnRequest("echo", func(_, body map[string]any) (any, error) {
				last, _ := lastSetting.Load().(string)
				return map[string]any{
					"value": body["value"], "app": h.AppID(), "lastSetting": last,
				}, nil
			})
			h.Log("virtual app ready")
			return nil
		},
		Drivers: map[string]appsdk.Driver{"switch": switchDriver{}},
	}
}
