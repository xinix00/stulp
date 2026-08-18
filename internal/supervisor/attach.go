package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
)

// Apps die zich zelf melden.
//
// De gewone weg is dat Stulp een app start: hij spawnt de binary en geeft hem één
// kant van een socketpair. Dat kan alleen als het proces van Stulp is.
//
// Attach is de andere weg. De app draait al -- als container, als systemd-unit,
// of onder een debugger -- en meldt zich op de attach-socket. Vanaf de handshake
// is er geen verschil meer: dezelfde frames over dezelfde soort socket, dezelfde
// runner in dezelfde map, en dus werkt alles wat er al was zonder dat het weet
// dat er twee manieren zijn.
//
// Wat wél verschilt is wie de app in de lucht houdt. Bij een gespawnde app is dat
// Stulp, met backoff en een herstartteller. Bij een aangemelde app is het degene
// die hem startte, en dan hóórt Stulp niets te doen: een herstart zou de binary
// naast app.json opstarten, naast het exemplaar dat er al is.

// waitingForAttach is wat er in Manage staat bij een app waar Stulp op wacht.
const waitingForAttach = "deze app start Stulp niet zelf; hij wacht tot de app zich meldt"

// attachedAppGone staat er als een aangemelde app wegvalt. Een andere zin dan
// waitingForAttach, want dit is een app die er wél was: er is iets gebeurd, en
// het antwoord op wat er nu gebeurt -- niets -- verdient een reden.
const attachedAppGone = "de app die zich gemeld had is weg; Stulp start hem niet in zijn plaats"

// attachGreetingTimeout begrenst hoe lang een verbinding mag zwijgen voordat hij
// zegt wie hij is. Daarna is het geen app die zich meldt maar een verbinding die
// blijft hangen. Over een poort dekt hij ook de TLS-handshake, die pas bij de
// eerste lezing gebeurt.
const attachGreetingTimeout = 10 * time.Second

// AttachOrigin zegt wat de verbinding zelf al bewees, en dus wat er nog te
// bewijzen valt.
type AttachOrigin int

const (
	// AttachLocal is een unix-socket. De kernel heeft de uid van de andere kant
	// nagerekend, en dat is een sterker bewijs dan een geheim: het valt niet te
	// kopiëren en het staat in geen image.
	AttachLocal AttachOrigin = iota
	// AttachRemote is een poort. Er is geen uid, dus geldt het token van de app --
	// en die reist binnen TLS, want een token dat iemand kan meelezen is geen
	// token meer.
	AttachRemote
)

// originOf leest van de verbinding zelf af wat hij bewijst, zodat een aanroeper
// dat niet kan verwarren. Een tls.Conn over TCP is geen *net.UnixConn, en een
// unix-socket kan niet van een andere machine komen.
func originOf(conn net.Conn) AttachOrigin {
	if _, unix := conn.(*net.UnixConn); unix {
		return AttachLocal
	}
	return AttachRemote
}

// ServeAttach neemt aanmeldingen aan tot de listener dichtgaat.
//
// Sluit de listener om te stoppen; dat is wat Accept laat terugkeren. De fout die
// daarbij hoort is geen fout en komt terug als nil.
func (s *Supervisor) ServeAttach(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		// Elke aanmelding in zijn eigen goroutine: de handshake van de een mag de
		// ander niet laten wachten, en een verbinding die zwijgt al helemaal niet.
		go s.accept(conn)
	}
}

// accept handelt één aanmelding af.
func (s *Supervisor) accept(raw net.Conn) {
	origin := originOf(raw)
	// Over een unix-socket eerst de uid, en dan pas lezen. Wie niet dezelfde
	// gebruiker is als Stulp krijgt geen byte van hem gelezen en geen antwoord dat
	// iets verraadt.
	if origin == AttachLocal {
		if err := appproto.CheckPeer(raw); err != nil {
			s.logger.Warn("attach refused", "error", err)
			raw.Close()
			return
		}
	}
	conn := appproto.NewConn(raw)
	if err := raw.SetDeadline(time.Now().Add(attachGreetingTimeout)); err != nil {
		conn.Close()
		return
	}
	// Stulp begint, met de nonce waarmee de app zich moet bewijzen. Die nonce is
	// van deze ene verbinding: een bewijs dat erbij hoort is nergens anders geldig,
	// en dus heeft wie meeleest er de volgende keer niets aan.
	//
	// Over een unix-socket blijft hij leeg. Daar heeft de kernel de vraag al
	// beantwoord, en een geheim erbij zou alleen iets zijn om kwijt te raken.
	nonce := ""
	if origin == AttachRemote {
		fresh, err := appproto.Nonce()
		if err != nil {
			s.logger.Error("attach nonce", "error", err)
			conn.Close()
			return
		}
		nonce = fresh
	}
	if err := appproto.Greet(conn, plugin.ProtocolVersion, nonce); err != nil {
		conn.Close()
		return
	}
	greeting, err := appproto.ReadAttach(conn)
	if err != nil {
		s.logger.Warn("attach greeting failed", "error", err)
		conn.Close()
		return
	}
	// De deadline hoorde bij de begroeting. Hierna is dit een langlevende
	// verbinding waarop lange stiltes normaal zijn -- een app die niets te melden
	// heeft, meldt niets.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}

	if err := s.attachWithNonce(context.Background(), greeting, conn, origin, nonce); err != nil {
		// De app-id staat erbij en het bewijs niet. Wat een aanmelding beweert mag in
		// de log; waarmee hij het onderbouwde niet.
		s.logger.Warn("attach failed", "app", greeting.AppID, "remote", origin == AttachRemote, "error", err)
		conn.Close()
		return
	}
	s.logger.Info("app attached", "app", greeting.AppID, "remote", origin == AttachRemote)
}

// Attach neemt een app aan die zich gemeld heeft.
//
// Attach antwoordt zelf op de begroeting -- goedgekeurd of geweigerd, met de
// reden erbij. Dat hoort hier omdat de volgorde hier zit: het antwoord moet weg
// zijn voordat de handshake begint, en er hoort er precies één te gaan. Een
// tweede zou bij de app aankomen als een frame dat hij niet kan lezen.
//
// De verbinding wordt van Stulp als dit nil teruggeeft. Komt er een fout uit, dan
// hoeft de aanroeper hem alleen nog te sluiten.
//
// Attach is de weg naar binnen voor een verbinding die zichzelf al bewezen heeft --
// een unix-socket, waar de kernel dat deed. Over een poort hoort attachWithNonce
// gebruikt te worden, want daar moet de app zich eerst bewijzen.
func (s *Supervisor) Attach(ctx context.Context, greeting appproto.Attach, conn *appproto.Conn, origin AttachOrigin) error {
	return s.attachWithNonce(ctx, greeting, conn, origin, "")
}

func (s *Supervisor) attachWithNonce(ctx context.Context, greeting appproto.Attach, conn *appproto.Conn, origin AttachOrigin, nonce string) error {
	appID := greeting.AppID
	// proof is wat Stulp aan een goedgekeurde app teruggeeft om te bewijzen dat hij
	// Stulp is. Leeg tot het bewijs van de app geklopt heeft: wie zich niet
	// aangemeld heeft, hoort ook niets over onze kant van het geheim te krijgen.
	proof := ""
	// refuse laat de andere kant weten waarom hij niet binnenkomt. Dat antwoord is
	// het enige wat hij te zien krijgt: wie een app in een container zet leest de
	// log van die container en niet die van Stulp.
	refuse := func(err error) error {
		_ = appproto.WriteAttachReply(conn, appproto.AttachReply{Error: err.Error()})
		return err
	}

	if greeting.Protocol != 0 && greeting.Protocol != plugin.ProtocolVersion {
		return refuse(fmt.Errorf("app %q speaks protocol %d, stulp speaks %d", appID, greeting.Protocol, plugin.ProtocolVersion))
	}
	s.mu.RLock()
	paused := s.paused
	s.mu.RUnlock()
	if paused {
		return refuse(errors.New("stulp is restoring a backup; retry shortly"))
	}
	// Over een poort moet de app bewijzen dat hij zijn token kent, en die controle
	// gaat vóór alles wat over deze app iets zou kunnen verraden. Eén melding voor
	// "deze app bestaat niet" en "dit bewijs klopt niet": wie het niet mag weten,
	// hoort ook niet te kunnen aftasten welke apps er zijn.
	if origin == AttachRemote {
		answer, err := s.checkAttachProof(ctx, greeting, nonce)
		if err != nil {
			return refuse(err)
		}
		proof = answer
	}
	// De app-id komt uit het document en niet uit de begroeting: de begroeting
	// zegt wie hij denkt te zijn, het document zegt of dat een app is.
	app, err := s.store.App(ctx, appID)
	if err != nil {
		// Onbekend, maar hij is hier gekomen én heeft zich bewezen: iemand heeft
		// deze app neergezet met een geldig token. Dat is genoeg om hem te
		// LATEN ZIEN en niet genoeg om hem te vertrouwen -- installeren blijft een
		// handeling van een mens, want een gelekt token mag geen sleutel tot het
		// huis zijn. Dus schrijven we hem op met het manifest dat hij meebracht
		// en sturen we hem weg met de reden.
		//
		// Zo is installeren op een node ook geen download meer: HOP plaatst het
		// image, de app meldt zich, en wat er nog rest is één klik.
		if offered := s.offerApp(ctx, greeting); offered != nil {
			return refuse(offered)
		}
		return refuse(fmt.Errorf("unknown app %q, and it sent no manifest to be offered with", appID))
	}
	announced, err := announcedManifest(greeting)
	if err != nil {
		return refuse(err)
	}
	// Een aangeboden of uitgezette app heeft geen runner waarmee deze
	// beschrijving kan botsen. Werk hem meteen bij, zodat Manage ook vóór de
	// volgende acceptatie/enable de drivers en UI van het geplaatste image ziet.
	if announced != nil && (app.Offered || !app.Enabled) {
		if _, err := s.store.UpdateAnnouncedApp(ctx, announced); err != nil {
			return refuse(fmt.Errorf("app %q could not update its announcement: %w", appID, err))
		}
		app, _ = s.store.App(ctx, appID)
	}
	// Aangeboden is niet geaccepteerd. Een app die zich elke paar seconden
	// opnieuw meldt (restart: always) krijgt dus elke keer dezelfde reden.
	if app.Offered {
		return refuse(fmt.Errorf("app %q announced itself and is waiting to be installed", appID))
	}
	// Een uitgezette app hoort niet te draaien, ook niet als hij zichzelf
	// aanbiedt. Dit is wat een container met "restart: always" tot bedaren
	// brengt: hij krijgt een reden en niet een dichte deur.
	if !app.Enabled {
		return refuse(fmt.Errorf("app %q is disabled", appID))
	}

	s.mu.Lock()
	if s.closed || s.paused {
		paused := s.paused
		s.mu.Unlock()
		if paused {
			return refuse(errors.New("stulp is restoring a backup; retry shortly"))
		}
		return refuse(errors.New("app supervisor is closed"))
	}
	// Al bezet. Geen overname: welk van de twee de echte is kan Stulp niet weten,
	// en de verkeerde wegsturen is erger dan de tweede weigeren. Wie een
	// draaiende app wil vervangen, stopt hem eerst.
	if s.runners[appID] != nil {
		s.mu.Unlock()
		return refuse(fmt.Errorf("app %q is already running", appID))
	}
	if s.starts[appID] != nil {
		s.mu.Unlock()
		return refuse(fmt.Errorf("app %q is already starting", appID))
	}
	// Dezelfde reservering als een gewone start, zodat twee apps die zich op
	// hetzelfde moment melden niet beide denken dat ze de eerste zijn.
	previous := s.states[appID]
	s.states[appID] = AppState{State: "starting", RestartCount: previous.RestartCount}
	startContext, cancel := context.WithCancel(ctx)
	start := &appStart{cancel: cancel}
	s.starts[appID] = start
	// Een app die zich meldt maakt een wachtende herstart overbodig.
	s.cancelRetryLocked(appID)
	s.mu.Unlock()

	// Pas nadat deze aanmelding zijn plek gereserveerd heeft wordt een nieuw
	// manifest voor een draaiende app vastgelegd. Zo kan een tweede exemplaar dat
	// straks als "already running" wordt geweigerd niet ondertussen de catalogus
	// van het eerste vervangen.
	if announced != nil {
		if _, err := s.store.UpdateAnnouncedApp(startContext, announced); err != nil {
			s.mu.Lock()
			if s.starts[appID] == start {
				delete(s.starts, appID)
				s.states[appID] = previous
			}
			s.mu.Unlock()
			cancel()
			return refuse(fmt.Errorf("app %q could not update its announcement: %w", appID, err))
		}
	}

	// Nu goedkeuren, en pas daarna beginnen. De app wacht op dit antwoord voordat
	// hij hello stuurt, en Start wacht op die hello -- in de andere volgorde
	// wachten ze op elkaar tot de timeout eraan komt.
	//
	// Het bewijs gaat mee: de app rekent hieraan na dat hij met Stulp praat en niet
	// met iemand die zijn plaats heeft ingenomen.
	if err := appproto.WriteAttachReply(conn, appproto.AttachReply{OK: true, Proof: proof}); err != nil {
		s.mu.Lock()
		if s.starts[appID] == start {
			delete(s.starts, appID)
			s.states[appID] = previous
		}
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("attach %q: %w", appID, err)
	}

	runner, err := s.newAttached(startContext, appID, conn)
	if err == nil {
		err = runner.Start(startContext)
	}
	return s.finishStart(appID, start, cancel, runner, err, true)
}

// checkAttachProof rekent na dat deze aanmelding van deze app komt, en levert het
// bewijs waarmee Stulp zich op zijn beurt aan die app bewijst.
//
// De fout is met opzet één zin voor drie gevallen: een app die niet bestaat, een
// bewijs dat niet klopt, en een Stulp die nog geen geheim heeft. Ze uit elkaar
// houden zou een poort veranderen in een manier om te ontdekken welke apps er in
// dit huis staan.
func (s *Supervisor) checkAttachProof(ctx context.Context, greeting appproto.Attach, nonce string) (string, error) {
	secret, err := s.store.AttachSecret(ctx)
	if err != nil {
		return "", errors.New("attach over a port is unavailable")
	}
	if !appproto.CheckProof(secret, greeting.AppID, nonce, greeting.Proof) {
		return "", errors.New("unknown app or wrong token")
	}
	// Terugbewijzen kan alleen als de app erom gevraagd heeft. Een app die geen
	// nonce stuurt neemt Stulp op zijn woord, en dat is zijn keuze -- maar de SDK
	// stuurt hem altijd.
	if greeting.Nonce == "" {
		return "", nil
	}
	return appproto.StulpProof(appproto.Token(secret, greeting.AppID), greeting.Nonce, greeting.AppID), nil
}

// offerApp schrijft een onbekende app op als aangeboden en geeft de reden die de
// app te horen krijgt. nil betekent: er was niets om aan te bieden (geen
// manifest), en dan valt de aanroeper terug op de gewone weigering.
//
// Het manifest komt uit de begroeting en wordt met dezelfde parser gelezen als
// een app.json op schijf: wat Stulp niet begrijpt, hoort hij ook niet op te
// schrijven.
func (s *Supervisor) offerApp(ctx context.Context, greeting appproto.Attach) error {
	announced, err := announcedManifest(greeting)
	if announced == nil && err == nil {
		return nil
	}
	if err != nil {
		return err
	}
	added, err := s.store.OfferApp(ctx, announced)
	if err != nil {
		return fmt.Errorf("app %q could not be offered: %w", greeting.AppID, err)
	}
	if added {
		s.logger.Info("app announced itself and is waiting to be installed",
			"app", announced.ID, "version", announced.Version)
	}
	return fmt.Errorf("app %q announced itself and is waiting to be installed", announced.ID)
}

func announcedManifest(greeting appproto.Attach) (*manifest.Manifest, error) {
	if len(greeting.Manifest) == 0 {
		return nil, nil
	}
	announced, err := manifest.Parse(greeting.Manifest)
	if err != nil {
		return nil, fmt.Errorf("app %q sent a manifest stulp cannot read: %w", greeting.AppID, err)
	}
	if announced.ID != greeting.AppID {
		// De id in het manifest is de echte; een begroeting die iets anders zegt
		// probeert zich als een andere app op te schrijven.
		return nil, fmt.Errorf("app %q sent a manifest for %q", greeting.AppID, announced.ID)
	}
	return announced, nil
}

// newAttached bouwt de runtime voor een aangemelde app. Een naad voor de tests,
// net als newRuntime: in productie is dit altijd het proces aan de andere kant
// van de verbinding.
func (s *Supervisor) newAttached(ctx context.Context, appID string, conn *appproto.Conn) (plugin.Runtime, error) {
	if s.newAttachedRuntime != nil {
		return s.newAttachedRuntime(ctx, appID, conn)
	}
	return plugin.NewAttached(ctx, s.store, appID, conn, s.options)
}
