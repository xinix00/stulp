package tahoma

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// box is een nagebouwde TaHoma-cloud: genoeg om te toetsen wat wij sturen en
// wat we met het antwoord doen.
type box struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recorded
	// logins telt geslaagde inlogpogingen. Dat aantal is het hele punt van de
	// sessietest: één keer opnieuw inloggen is herstel, tien keer is een lus.
	logins int
	// valid staat op onwaar zolang de sessie verlopen is; login zet hem terug.
	valid bool
	// setup is wat GET /setup antwoordt. Een test verzet hem tussen rondes door.
	setup string
	// deny laat login mislukken, voor een wachtwoord dat niet deugt.
	deny bool
}

type recorded struct {
	method string
	path   string
	form   string
	body   string
	cookie string
}

func newBox(t *testing.T) *box {
	t.Helper()
	b := &box{setup: `{"devices":[]}`}
	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.Close)
	return b
}

func (b *box) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	entry := recorded{method: r.Method, path: r.URL.Path, body: string(raw)}
	if cookie, err := r.Cookie("JSESSIONID"); err == nil {
		entry.cookie = cookie.Value
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		entry.form = string(raw)
	}

	b.mu.Lock()
	b.requests = append(b.requests, entry)
	login := r.URL.Path == "/login"
	deny, valid := b.deny, b.valid
	if login && !deny {
		b.logins++
		b.valid = true
	}
	setup := b.setup
	b.mu.Unlock()

	if login {
		if deny {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"Bad credentials"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "sessie", Path: "/"})
		fmt.Fprint(w, `{"success":true}`)
		return
	}

	// Alles buiten /login vraagt een geldige sessie. Dit is hoe TaHoma een
	// verlopen sessie meldt: geen bijzonder veld, gewoon 401 op een gewoon
	// verzoek.
	if !valid {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"Not authenticated"}`)
		return
	}

	switch r.URL.Path {
	case "/setup":
		fmt.Fprint(w, setup)
	case "/exec/apply":
		fmt.Fprint(w, `{"execId":"exec-1"}`)
	case "/actionGroups":
		fmt.Fprint(w, `[{"oid":"s1","label":"Alles dicht"}]`)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// expire laat de volgende aanroep op een verlopen sessie stuiten.
func (b *box) expire() {
	b.mu.Lock()
	b.valid = false
	b.mu.Unlock()
}

func (b *box) setSetup(body string) {
	b.mu.Lock()
	b.setup = body
	b.mu.Unlock()
}

func (b *box) loginCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logins
}

func (b *box) taken() []recorded {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recorded(nil), b.requests...)
}

func (b *box) client() *Client {
	client := New("iemand@example.com", "geheim")
	client.Base = b.URL
	client.HTTP.Timeout = 5 * time.Second
	return client
}

const twoShutters = `{"devices":[
  {"deviceURL":"io://1234-5678-9012/1","oid":"oid-1","label":"Woonkamer",
   "controllableName":"io:RollerShutterGenericIOComponent",
   "states":[{"name":"core:OpenClosedState","value":"open"},{"name":"core:ClosureState","value":0}]},
  {"deviceURL":"io://1234-5678-9012/2","oid":"oid-2","label":"Keuken",
   "controllableName":"io:HorizontalAwningIOComponent",
   "states":[{"name":"core:OpenClosedState","value":"closed"},{"name":"core:ClosureState","value":100}]}
]}`

// Inloggen gaat met een formulier en met de veldnamen die Somfy verwacht. Een
// tikfout daarin levert een 401 op die er precies uitziet als een verkeerd
// wachtwoord, en dan zoekt iemand op de verkeerde plek.
func TestLoginPostsTheFormSomfyExpects(t *testing.T) {
	box := newBox(t)
	if err := box.client().Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	taken := box.taken()
	if len(taken) != 1 {
		t.Fatalf("aantal verzoeken = %d", len(taken))
	}
	got := taken[0]
	if got.method != http.MethodPost || got.path != "/login" {
		t.Fatalf("inloggen ging via %s %s", got.method, got.path)
	}
	if got.form != "userId=iemand%40example.com&userPassword=geheim" {
		t.Fatalf("formulier = %q", got.form)
	}
}

// Een wachtwoord dat niet deugt hoort een melding op te leveren waar iemand
// iets aan heeft, en geen eindeloos opnieuw proberen.
func TestWrongPasswordFailsLoudlyAndOnce(t *testing.T) {
	box := newBox(t)
	box.deny = true
	err := box.client().Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "iemand@example.com") {
		t.Fatalf("verkeerd wachtwoord gaf %v", err)
	}
	if box.loginCount() != 0 {
		t.Fatalf("er is een sessie ontstaan uit een mislukte inlog")
	}
}

// De sessie verloopt vanzelf en dat hoort niemand te merken: TaHoma antwoordt
// 401 op een gewoon verzoek, en dan wordt er opnieuw ingelogd en gaat het
// verzoek over. Dit is het gedrag uit lib/HttpHelper.js (reAuthenticate).
func TestExpiredSessionRenewsItself(t *testing.T) {
	box := newBox(t)
	client := box.client()
	box.setSetup(twoShutters)

	if _, err := client.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if box.loginCount() != 1 {
		t.Fatalf("eerste ophaling logde %d keer in", box.loginCount())
	}

	box.expire()
	setup, err := client.Setup(context.Background())
	if err != nil {
		t.Fatalf("een verlopen sessie werd een fout: %v", err)
	}
	if len(setup.Devices) != 2 {
		t.Fatalf("na vernieuwen kwamen er %d apparaten", len(setup.Devices))
	}
	if box.loginCount() != 2 {
		t.Fatalf("aantal inlogpogingen = %d, wil 2", box.loginCount())
	}

	// En het verzoek is echt herhaald: 401, login, en dan nog een keer /setup.
	taken := box.taken()
	tail := taken[len(taken)-3:]
	if tail[0].path != "/setup" || tail[1].path != "/login" || tail[2].path != "/setup" {
		t.Fatalf("volgorde na verlopen sessie = %v", []string{tail[0].path, tail[1].path, tail[2].path})
	}
	if tail[2].cookie == "" {
		t.Fatalf("het herhaalde verzoek droeg geen sessiecookie")
	}
}

// Blijft TaHoma weigeren ná het opnieuw inloggen, dan is het geen verlopen
// sessie maar een account dat niet meer klopt. Dan hoort het te stoppen met een
// melding, en niet in een lus te gaan.
func TestSecondRejectionStopsInsteadOfLooping(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "sessie", Path: "/"})
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := New("iemand", "geheim")
	client.Base = server.URL
	_, err := client.Setup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gebruikersnaam en wachtwoord") {
		t.Fatalf("tweede weigering gaf %v", err)
	}
	if attempts != 2 {
		t.Fatalf("/setup is %d keer geprobeerd, wil 2", attempts)
	}
}

// Tien apparaten die tegelijk op een verlopen sessie stuiten horen samen één
// keer in te loggen. Tien keer achter elkaar inloggen is precies het gedrag
// waarop een cloud je gaat weigeren.
func TestConcurrentExpiryLogsInOnce(t *testing.T) {
	box := newBox(t)
	client := box.client()
	box.setSetup(twoShutters)
	if _, err := client.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	box.expire()

	var wait sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := client.Setup(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("gelijktijdige ophaling gaf %v", err)
	}
	if got := box.loginCount(); got != 2 {
		t.Fatalf("aantal inlogpogingen = %d, wil 2 (de eerste plus één vernieuwing)", got)
	}
}

// De poll levert standwijzigingen op, en alleen die. Een rolluik dat stilstaat
// hoort niet elke ronde opnieuw dezelfde waarde af te geven.
func TestPollReportsOnlyWhatChanged(t *testing.T) {
	box := newBox(t)
	box.setSetup(twoShutters)

	var mu sync.Mutex
	var seen []Device
	poller := NewPoller(box.client(), time.Hour, Handlers{
		OnDevice: func(device Device) {
			mu.Lock()
			seen = append(seen, device)
			mu.Unlock()
		},
		OnError: func(err error) { t.Error(err) },
	})

	ctx := context.Background()
	poller.round(ctx)
	if len(seen) != 2 {
		t.Fatalf("de eerste ronde meldde %d apparaten, wil 2", len(seen))
	}

	seen = nil
	poller.round(ctx)
	if len(seen) != 0 {
		t.Fatalf("een ronde zonder wijziging meldde %d apparaten", len(seen))
	}

	// Het rolluik in de woonkamer gaat halfdicht.
	box.setSetup(strings.Replace(twoShutters,
		`{"name":"core:ClosureState","value":0}`,
		`{"name":"core:ClosureState","value":40}`, 1))
	seen = nil
	poller.round(ctx)
	if len(seen) != 1 {
		t.Fatalf("een gewijzigde stand meldde %d apparaten, wil 1", len(seen))
	}
	changed := seen[0]
	if changed.DeviceURL != "io://1234-5678-9012/1" {
		t.Fatalf("het verkeerde apparaat veranderde: %s", changed.DeviceURL)
	}
	closure, ok := changed.Number(StateClosure)
	if !ok || closure != 40 {
		t.Fatalf("sluiting = %v %v", closure, ok)
	}
	// En dit is waar de tegel op komt te staan: 40% dicht is 60% open.
	if position := Position(closure); position != 0.6 {
		t.Fatalf("windowcoverings_set = %v, wil 0.6", position)
	}
}

// Een apparaat dat uit de doos verdwijnt en terugkomt telt weer als nieuw:
// anders blijft zijn tegel op de oude waarde staan tot hij toevallig beweegt.
func TestDeviceThatReturnsIsReportedAgain(t *testing.T) {
	box := newBox(t)
	box.setSetup(twoShutters)
	var seen []Device
	poller := NewPoller(box.client(), time.Hour, Handlers{
		OnDevice: func(device Device) { seen = append(seen, device) },
		OnError:  func(err error) { t.Error(err) },
	})
	ctx := context.Background()
	poller.round(ctx)

	box.setSetup(`{"devices":[]}`)
	poller.round(ctx)

	box.setSetup(twoShutters)
	seen = nil
	poller.round(ctx)
	if len(seen) != 2 {
		t.Fatalf("na terugkeer meldde de poll %d apparaten, wil 2", len(seen))
	}
}

// De as van Somfy loopt tegen die van Stulp in. Dit is de omrekening waar het om
// draait; hem verkeerd om hebben betekent dat elk rolluik in huis andersom
// reageert dan de gebruiker vraagt.
func TestClosureAndPositionAreOppositeAxes(t *testing.T) {
	for _, test := range []struct {
		closure  float64
		position float64
	}{
		{0, 1},    // niets dicht -> helemaal open
		{100, 0},  // helemaal dicht -> dicht
		{40, 0.6}, // 40% dicht -> 60% open
		{75, 0.25},
	} {
		if got := Position(test.closure); got != test.position {
			t.Errorf("Position(%v) = %v, wil %v", test.closure, got, test.position)
		}
		if got := Closure(test.position); got != int(test.closure) {
			t.Errorf("Closure(%v) = %d, wil %v", test.position, got, test.closure)
		}
	}
}

// Buiten 0..1 hoort geen setClosure van 140 uit te komen.
func TestPositionsOutsideTheSliderAreClamped(t *testing.T) {
	for _, test := range []struct {
		position float64
		closure  int
		// 99,5 wordt 100, net als Math.round in de bron: halve procenten gaan
		// omhoog, en beide kanten doen dat hetzelfde.
	}{{-1, 100}, {2, 0}, {0.005, 100}, {0.994, 1}} {
		if got := Closure(test.position); got != test.closure {
			t.Errorf("Closure(%v) = %d, wil %d", test.position, got, test.closure)
		}
	}
	if got := Position(140); got != 0 {
		t.Errorf("Position(140) = %v, wil 0", got)
	}
}

// Een luifel gaat naar beneden als hij opengaat, en dat draait de commando's om
// zonder de sluiting om te draaien.
func TestAwningRunsTheOtherWay(t *testing.T) {
	if command, _ := Shutter.Command(StateUp); command != CommandOpen {
		t.Errorf("rolluik omhoog = %q", command)
	}
	if command, _ := Awning.Command(StateUp); command != CommandClose {
		t.Errorf("luifel omhoog = %q", command)
	}
	if _, ok := Shutter.Command(StateIdle); ok {
		t.Error("idle hoort geen commando te hebben; stoppen is een uitvoering intrekken")
	}
	// De sluiting blijft voor beide dezelfde spiegeling.
	if Position(100) != Position(100) || Closure(0) != 100 {
		t.Error("de sluiting hoort voor luifels niet anders te rekenen")
	}
}

// Halverwege is idle, en de open/dicht-tekst betekent per soort iets anders.
func TestMotionStateFollowsTheClosure(t *testing.T) {
	for _, test := range []struct {
		name       string
		direction  Direction
		openClosed string
		closure    float64
		has        bool
		want       string
	}{
		{"rolluik open", Shutter, "open", 0, true, StateUp},
		{"rolluik dicht", Shutter, "closed", 100, true, StateDown},
		{"rolluik halverwege", Shutter, "open", 40, true, StateIdle},
		{"luifel open", Awning, "open", 0, true, StateDown},
		{"luifel dicht", Awning, "closed", 100, true, StateUp},
		{"luifel halverwege", Awning, "open", 60, true, StateIdle},
		{"zonder sluiting", Shutter, "closed", 0, false, StateDown},
	} {
		got, ok := test.direction.Motion(test.openClosed, test.closure, test.has)
		if !ok || got != test.want {
			t.Errorf("%s = %q %v, wil %q", test.name, got, ok, test.want)
		}
	}
	// Een open/dicht-stand die TaHoma niet als open of closed meldt is geen
	// stand die we mogen raden.
	if _, ok := Shutter.Motion("unknown", 0, false); ok {
		t.Error("een onbekende open/dicht-stand werd toch vertaald")
	}
}

// Een opdracht levert een uitvoer-id op, en dat is het handvat om hem te
// stoppen. Zonder id is er niets te stoppen en dat hoort een fout te zijn.
func TestExecuteReturnsTheExecutionToCancel(t *testing.T) {
	box := newBox(t)
	client := box.client()
	execution, err := client.Execute(context.Background(), "Woonkamer", "io://1/1",
		Command{Name: CommandSetClosure, Parameters: []any{40}})
	if err != nil {
		t.Fatal(err)
	}
	if execution != "exec-1" {
		t.Fatalf("uitvoer-id = %q", execution)
	}
	taken := box.taken()
	last := taken[len(taken)-1]
	if last.path != "/exec/apply" {
		t.Fatalf("opdracht ging naar %s", last.path)
	}
	for _, want := range []string{`"deviceURL":"io://1/1"`, `"name":"setClosure"`, `"parameters":[40]`} {
		if !strings.Contains(last.body, want) {
			t.Fatalf("de opdracht miste %s: %s", want, last.body)
		}
	}
}

func TestExecuteWithoutExecutionIDFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client := New("iemand", "geheim")
	client.Base = server.URL
	_, err := client.Execute(context.Background(), "Woonkamer", "io://1/1",
		Command{Name: CommandOpen})
	if err == nil || !strings.Contains(err.Error(), "uitvoer-id") {
		t.Fatalf("een antwoord zonder execId gaf %v", err)
	}
}

// Een 200 met success:false is bij TaHoma een mislukte inlog. De bron kijkt naar
// dat veld en niet naar de statuscode; wie dat overslaat draait door op een
// sessie die er niet is.
func TestSuccessFalseIsNotALogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"error":"Too many requests"}`)
	}))
	defer server.Close()
	client := New("iemand", "geheim")
	client.Base = server.URL
	err := client.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Too many requests") {
		t.Fatalf("success:false gaf %v", err)
	}
}
