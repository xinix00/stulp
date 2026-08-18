package stats

import (
	"strings"
	"sync"
	"time"

	"github.com/xinix00/stulp/internal/store"
)

// De verzamelaar luistert mee op wat er in huis verandert.
//
// Hij vraagt niets en pollt niets: elke wijziging komt toch al langs de
// gebeurtenisbus die Manage ook gebruikt. Statistiek bijhouden kost daarmee geen
// enkel netwerkverkeer en geen enkele extra vraag aan een apparaat.

// Collector houdt de reeksen van alle apparaten bij.
type Collector struct {
	mu     sync.RWMutex
	series map[string]*Series // "deviceID capability"
	names  map[string]string  // deviceID -> naam, voor de leesbaarheid

	stop chan struct{}
	done chan struct{}
}

// New maakt een verzamelaar die nog niet luistert.
func New() *Collector {
	return &Collector{series: map[string]*Series{}, names: map[string]string{}}
}

// closeEvery is hoe vaak lopende standen afgesloten worden.
//
// Een deur die dicht blijft stuurt niets, en zonder deze tik zou "hoe lang stond
// hij dicht" pas meetellen als hij weer opengaat. Vijf minuten is fijner dan het
// kleinste vak van tien, zodat elk vak minstens één keer bijgewerkt wordt.
const closeEvery = 5 * time.Minute

// Start begint te luisteren en houdt dat vol tot Close.
func (c *Collector) Start(database *store.Store) {
	c.mu.Lock()
	if c.stop != nil {
		// Al bezig. Twee luisteraars op dezelfde gebeurtenissen zouden elke
		// meting dubbel tellen.
		c.mu.Unlock()
		return
	}
	events, cancel := database.Subscribe(256)
	stop, done := make(chan struct{}), make(chan struct{})
	c.stop, c.done = stop, done
	c.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(closeEvery)
		defer ticker.Stop()
		for {
			select {
			case event, open := <-events:
				if !open {
					return
				}
				c.observe(event, time.Now())
			case now := <-ticker.C:
				c.closeAll(now)
			case <-stop:
				return
			}
		}
	}()
}

func (c *Collector) Close() {
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.stop, c.done = nil, nil
	c.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// Running zegt of er nu meegelezen wordt.
func (c *Collector) Running() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stop != nil
}

// Forget gooit weg wat er verzameld is. Bij het uitzetten: het geheugen
// vasthouden van iets dat niet meer bijgewerkt wordt is het slechtste van twee
// werelden -- het kost evenveel en het klopt niet meer.
func (c *Collector) Forget() {
	c.mu.Lock()
	c.series = map[string]*Series{}
	c.names = map[string]string{}
	c.mu.Unlock()
}

// observe verwerkt één gebeurtenis.
func (c *Collector) observe(event store.Event, at time.Time) {
	if event.Manager != "devices" || event.Type != "device.update" {
		return
	}
	device, ok := event.Data.(store.Device)
	if !ok {
		return
	}
	c.mu.Lock()
	c.names[device.ID] = device.Name
	c.mu.Unlock()
	for capability, value := range device.State {
		kind, ok := KindOf(capability)
		if !ok {
			continue
		}
		number, ok := number(value)
		if !ok {
			continue
		}
		c.seriesFor(device.ID, capability, kind).Add(number, at)
	}
}

// KindOf zegt hoe een capability samengevat hoort te worden.
//
// Op naam en niet op waarde: de naam zegt wat het bétekent. measure_temperature
// is een meting die op en neer gaat, meter_power een teller die alleen oploopt,
// en alarm_contact een stand die aan of uit staat. Alleen naar de waarde kijken
// zou een teller van 21,5 niet van een temperatuur van 21,5 kunnen scheiden.
func KindOf(capability string) (Kind, bool) {
	name, _, _ := strings.Cut(capability, ".")
	switch {
	case strings.HasPrefix(name, "meter_"):
		return Counter, true
	case strings.HasPrefix(name, "measure_"), name == "dim", name == "volume_set",
		name == "light_hue", name == "light_saturation", name == "target_temperature":
		return Gauge, true
	case strings.HasPrefix(name, "alarm_"), name == "onoff", name == "locked":
		return Fraction, true
	}
	return 0, false
}

// number maakt er een getal van. true en false tellen als één en nul, want dat
// is precies wat een breuk nodig heeft.
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func (c *Collector) seriesFor(deviceID, capability string, kind Kind) *Series {
	key := deviceID + " " + capability
	c.mu.RLock()
	series := c.series[key]
	c.mu.RUnlock()
	if series != nil {
		return series
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if series = c.series[key]; series == nil {
		series = NewSeries(kind)
		c.series[key] = series
	}
	return series
}

func (c *Collector) closeAll(now time.Time) {
	c.mu.RLock()
	all := make([]*Series, 0, len(c.series))
	for _, series := range c.series {
		all = append(all, series)
	}
	c.mu.RUnlock()
	for _, series := range all {
		series.Close(now)
	}
}

// Series levert één reeks, als hij bestaat.
func (c *Collector) Series(deviceID, capability string) (*Series, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	series, ok := c.series[deviceID+" "+capability]
	return series, ok
}

// Entry beschrijft één reeks in de lijst.
type Entry struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName,omitempty"`
	Capability string `json:"capability"`
	Kind       string `json:"kind"`
	Bytes      int    `json:"bytes"`
}

// List levert wat er bijgehouden wordt, met wat het kost.
func (c *Collector) List() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, 0, len(c.series))
	for key, series := range c.series {
		deviceID, capability, _ := strings.Cut(key, " ")
		out = append(out, Entry{
			DeviceID: deviceID, DeviceName: c.names[deviceID], Capability: capability,
			Kind: series.Kind.String(), Bytes: series.Bytes(),
		})
	}
	return out
}

// Bytes is wat de hele verzameling kost. Eén getal, altijd op te vragen: de
// belofte dat dit goedkoop is hoort na te rekenen te zijn en niet geloofd.
func (c *Collector) Bytes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, series := range c.series {
		total += series.Bytes()
	}
	return total
}

func (k Kind) String() string {
	switch k {
	case Counter:
		return "counter"
	case Fraction:
		return "fraction"
	}
	return "gauge"
}
