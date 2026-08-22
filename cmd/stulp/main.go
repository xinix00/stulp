//go:build !tamago

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/xinix00/stulp/internal/appinstall"
	"github.com/xinix00/stulp/internal/appproto"
	"github.com/xinix00/stulp/internal/backup"
	"github.com/xinix00/stulp/internal/imageshare"
	"github.com/xinix00/stulp/internal/manifest"
	"github.com/xinix00/stulp/internal/plugin"
	"github.com/xinix00/stulp/internal/stats"
	"github.com/xinix00/stulp/internal/store"
	"github.com/xinix00/stulp/internal/supervisor"
	"github.com/xinix00/stulp/internal/webapi"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "stulp:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("stulp", flag.ContinueOnError)
	global.SetOutput(stderr)
	documentPath := global.String("document", "stulp.json", "path to the Stulp document")
	// Nederlands als standaard, want dat is wat Manage zelf spreekt: elke knop
	// en elke melding in de interface staat in het Nederlands. Met "en" kwamen
	// de titels van Flow-kaarten er als enige in het Engels tussen, en dan lijkt
	// een kaart die je zoekt er niet te zijn.
	language := global.String("language", "nl", "app language")
	timezone := global.String("timezone", "UTC", "timezone")
	if err := global.Parse(arguments); err != nil {
		return err
	}
	args := global.Args()
	if len(args) == 0 {
		usage(stdout)
		return nil
	}
	if args[0] == "restore" {
		if len(args) != 2 {
			return errors.New("usage: stulp restore BACKUP.zip")
		}
		result, err := backup.Restore(context.Background(), args[1], *documentPath)
		if err != nil {
			return err
		}
		return printJSON(stdout, result)
	}

	database, err := store.Open(*documentPath)
	if err != nil {
		return err
	}
	defer database.Close()
	ctx := context.Background()

	switch args[0] {
	case "backup":
		if len(args) != 2 {
			return errors.New("usage: stulp backup OUTPUT.zip")
		}
		if err := backup.WriteFile(ctx, database, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "backup written to %s\n", args[1])
		return nil

	case "install":
		if len(args) != 2 {
			return errors.New("usage: stulp install APP_DIRECTORY")
		}
		// Alleen een map: er wordt niets meer gedownload. Een app die niet naast
		// je staat komt binnen doordat iemand hem neerzet -- HOP plaatst een
		// slot-image, docker start een container -- en zich daarna meldt met zijn
		// manifest. Dan staat hij als aangeboden in het document en installeer je
		// hem met één handeling in Manage.
		appManifest, root, err := manifest.Load(args[1])
		if err != nil {
			return err
		}
		if err := database.InstallApp(ctx, appManifest, root, ""); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "installed %s@%s from %s\n", appManifest.ID, appManifest.Version, root)
		return nil

	// De commando's check-updates en update stonden hier. Ze vroegen GitHub om een
	// nieuwere versie, en die weg bestaat niet meer: een app wordt bijgewerkt door
	// een nieuw image neer te zetten, en hij meldt zijn versie zelf als hij zich
	// aanmeldt.

	case "uninstall":
		if len(args) != 2 {
			return errors.New("usage: stulp uninstall APP_ID")
		}
		app, devices, err := database.UninstallApp(ctx, args[1])
		if err != nil {
			return err
		}
		deviceIDs := make([]string, 0, len(devices))
		for _, device := range devices {
			deviceIDs = append(deviceIDs, device.ID)
		}
		disabled, flowErr := database.DisableFlowsFor(ctx, app, deviceIDs)
		fmt.Fprintf(stdout, "uninstalled %s@%s, removed %d device(s), disabled %d flow(s)\n",
			app.ID, app.Version, len(devices), len(disabled))
		if flowErr != nil {
			return flowErr
		}
		appsRoot, rootErr := database.AppsRoot()
		if rootErr != nil {
			return nil
		}
		removed, removeErr := appinstall.RemoveBundle(appsRoot, app.Root)
		if removeErr != nil {
			return removeErr
		}
		if !removed && app.Root != "" {
			fmt.Fprintf(stdout, "left %s in place: it is not a bundle Stulp downloaded\n", app.Root)
		}
		return nil

	case "apps":
		apps, err := database.Apps(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, apps)

	case "devices":
		appID := ""
		if len(args) > 1 {
			appID = args[1]
		}
		devices, err := database.Devices(ctx, appID)
		if err != nil {
			return err
		}
		return printJSON(stdout, devices)

	case "add-device":
		return addDevice(ctx, database, args[1:], stdout, stderr)

	case "run":
		command := flag.NewFlagSet("run", flag.ContinueOnError)
		command.SetOutput(stderr)
		once := command.Bool("once", false, "initialize the app and exit")
		if err := command.Parse(args[1:]); err != nil {
			return err
		}
		if command.NArg() != 1 {
			return errors.New("usage: stulp run [--once] APP_ID")
		}
		runner, err := startRunner(ctx, database, command.Arg(0), *language, *timezone)
		if err != nil {
			return err
		}
		defer runner.Close()
		if *once {
			fmt.Fprintf(stdout, "started %s successfully\n", command.Arg(0))
			return nil
		}
		fmt.Fprintf(stdout, "%s is running; press Ctrl-C to stop\n", command.Arg(0))
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		<-signals
		return nil

	case "serve":
		command := flag.NewFlagSet("serve", flag.ContinueOnError)
		command.SetOutput(stderr)
		listen := command.String("listen", "127.0.0.1:8080", "HTTP listen address")
		token := command.String("token", "", "access key used in /<key> and /mcp/<key>")
		logLevel := command.String("log-level", "info", "debug, info, warn or error")
		// Push werkt alleen in een beveiligde context. Een browser weigert een
		// service worker over gewone http op een LAN-adres, dus zonder deze twee
		// vlaggen (of een https-proxy ervoor) blijft die knop in Manage grijs.
		tlsCert := command.String("tls-cert", "", "certificate chain for https, required for push notifications")
		tlsKey := command.String("tls-key", "", "private key belonging to --tls-cert")
		// Zonder deze vlag start Stulp elke app zelf en is er geen adres waarop een
		// app zich kan melden. Dat is de standaard omdat een socket die er niet is
		// niets is om binnen te komen; wie een app in een container zet of onder een
		// debugger houdt, vraagt erom.
		attach := command.String("attach", "", "unix socket where an already running app may attach itself")
		// Een poort is de enige weg voor een app op een andere machine of in een
		// eigen pod, en hij kost wat de socket niet kostte: een token per app.
		attachPort := command.String("attach-port", "", "host:port where an app may attach, proving it knows its token")
		// Zonder TLS blijft het bewijs met de nonce staan, dus komt er nog steeds
		// niemand binnen die het token niet kent. Wat wegvalt is geheimhouding: alles
		// na de begroeting ligt open. Vandaar deze naam en niet "insecure" -- het is
		// niet onbeveiligd, het is onversleuteld, en dat is een ander gesprek.
		attachPlaintext := command.Bool("attach-plaintext", false, "run --attach-port without TLS: apps still prove their token, but everything after the handshake is readable on the wire")
		if err := command.Parse(args[1:]); err != nil {
			return err
		}
		if command.NArg() != 0 {
			return errors.New("usage: stulp serve [--listen ADDRESS] [--token KEY] [--log-level LEVEL] [--tls-cert FILE --tls-key FILE] [--attach SOCKET] [--attach-port ADDRESS]")
		}
		if (*tlsCert == "") != (*tlsKey == "") {
			return errors.New("--tls-cert and --tls-key belong together")
		}
		// Fail closed. Een poort zonder TLS is een token dat over het netwerk in het
		// open ligt, en wie dat wil hoort het te zeggen in plaats van het te krijgen
		// omdat hij een certificaat vergat.
		if *attachPort != "" && *tlsCert == "" && !*attachPlaintext {
			return errors.New("--attach-port needs --tls-cert and --tls-key, or --attach-plaintext to say you mean it")
		}
		level, err := parseLogLevel(*logLevel)
		if err != nil {
			return err
		}
		logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
		runtimeOptions := plugin.Options{
			Language: *language, Timezone: *timezone, StulpID: "stulp-local",
			StulpVersion: version, Logger: logger,
		}
		appsSupervisor := supervisor.New(database, runtimeOptions)
		defer appsSupervisor.Close()
		// Eén plek waar een gedeelde afbeelding wacht: de supervisor legt hem erin
		// als een plugin erom vraagt, de interface levert hem uit. In het geheugen,
		// begrensd, en na een kwartier weg.
		images := imageshare.New()
		appsSupervisor.UseImages(images)
		// De statistiek luistert mee op dezelfde gebeurtenissen als Manage. Hij
		// vraagt niets aan een apparaat en schrijft niets naar schijf; bij het
		// stoppen is hij weg, en dat is de afspraak.
		//
		// Het hoort een keuze te zijn: hij kost geheugen zolang Stulp draait, en
		// een installatie die er niet naar kijkt hoort er niet voor te betalen.
		// Die keuze staat in het document en niet in een vlag -- een vlag vraagt
		// een herstart met andere argumenten, en dat is geen knop.
		statistics := stats.New()
		defer statistics.Close()
		if system, err := database.System(ctx); err == nil && system.Statistics {
			statistics.Start(database)
		} else if err != nil {
			fmt.Fprintf(stderr, "stulp: could not read the system settings: %v\n", err)
		}
		// De attach-wegen voordat de apps starten: een app die "external" in zijn
		// app.json heeft gaat bij StartAll naar wachten, en dan hoort de deur al
		// open te staan in plaats van een seconde later.
		serveAttach := func(listener net.Listener, what string) {
			go func() {
				if err := appsSupervisor.ServeAttach(listener); err != nil {
					logger.Error("attach listener stopped", "on", what, "error", err)
				}
			}()
			fmt.Fprintf(stdout, "Apps may attach on %s\n", what)
		}
		if *attach != "" {
			listener, err := appproto.Listen(*attach)
			if err != nil {
				return err
			}
			defer listener.Close()
			serveAttach(listener, *attach)
		}
		if *attachPort != "" {
			listener, err := net.Listen("tcp", *attachPort)
			if err != nil {
				return err
			}
			defer listener.Close()
			description := *attachPort + " (TLS)"
			if *tlsCert != "" && !*attachPlaintext {
				certificate, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
				if err != nil {
					return fmt.Errorf("attach certificate: %w", err)
				}
				listener = tls.NewListener(listener, &tls.Config{
					Certificates: []tls.Certificate{certificate},
					MinVersion:   tls.VersionTLS13,
				})
			} else {
				description = *attachPort + " (no TLS; apps still prove their token, but the traffic is readable)"
				logger.Warn("attach port has no TLS: apps still prove their token, but device data and app settings are readable on the wire",
					"address", *attachPort)
			}
			serveAttach(listener, description)
		}
		if err := appsSupervisor.StartAll(ctx); err != nil {
			fmt.Fprintf(stderr, "stulp: one or more apps failed to start: %v\n", err)
		}
		api := webapi.New(database, appsSupervisor, webapi.Options{
			StulpID: runtimeOptions.StulpID, StulpVersion: runtimeOptions.StulpVersion,
			Language: *language, Timezone: *timezone, Token: *token, Logger: logger,
		})
		api.UseStatistics(statistics)
		api.UseImages(images)
		defer api.Close()
		httpServer := &http.Server{
			Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second,
		}
		serverErrors := make(chan error, 1)
		scheme := "http"
		if *tlsCert != "" {
			scheme = "https"
			go func() { serverErrors <- httpServer.ListenAndServeTLS(*tlsCert, *tlsKey) }()
		} else {
			go func() { serverErrors <- httpServer.ListenAndServe() }()
		}
		fmt.Fprintf(stdout, "Stulp Manage and MCP listening on %s://%s\n", scheme, *listen)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		select {
		case err := <-serverErrors:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-signals:
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdownContext)
		}

	case "attach-token":
		// Het token van een app, om in een Secret of een unit-file te zetten.
		//
		// Het wordt niet bewaard maar herleid: één geheim in het document, HMAC over
		// de app-id. Dus levert dit commando twee keer hetzelfde token op, en is er
		// niets kwijt te raken behalve dat ene geheim.
		command := flag.NewFlagSet("attach-token", flag.ContinueOnError)
		command.SetOutput(stderr)
		rotate := command.Bool("rotate", false, "throw the secret away, so every token stops working")
		if err := command.Parse(args[1:]); err != nil {
			return err
		}
		if *rotate {
			if command.NArg() != 0 {
				return errors.New("usage: stulp attach-token --rotate")
			}
			system, err := database.System(ctx)
			if err != nil {
				return err
			}
			system.AttachSecret = ""
			if err := database.SetSystem(ctx, system); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "the attach secret is gone; every token that used it stops working")
			return nil
		}
		if command.NArg() != 1 {
			return errors.New("usage: stulp attach-token APP_ID")
		}
		appID := command.Arg(0)
		// Een token voor een app die Stulp nog niet kent is het normale geval: dit
		// is wat je meegeeft aan een app die je gaat NEERZETTEN, en hij kan zich
		// pas melden als hij dat token heeft. Dus geen fout, maar wel een regel
		// erbij -- want wie hier een typefout maakt, zoekt straks naar een
		// aanmelding die nooit komt.
		if _, err := database.App(ctx, appID); err != nil {
			fmt.Fprintf(stderr, "note: no app %q is known yet; this token becomes usable when it announces itself\n", appID)
		}
		secret, err := database.AttachSecret(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, appproto.Token(secret, appID))
		return nil

	case "inspect":
		if len(args) != 2 {
			return errors.New("usage: stulp inspect APP_ID")
		}
		runner, err := startRunner(ctx, database, args[1], *language, *timezone)
		if err != nil {
			return err
		}
		defer runner.Close()
		registrations, err := runner.Registrations(ctx)
		if err != nil {
			return err
		}
		return printJSON(stdout, registrations)

	case "pair-list":
		if len(args) != 3 {
			return errors.New("usage: stulp pair-list APP_ID DRIVER_ID")
		}
		runner, err := startRunner(ctx, database, args[1], *language, *timezone)
		if err != nil {
			return err
		}
		defer runner.Close()
		devices, err := runner.PairListDevices(ctx, args[2])
		if err != nil {
			return err
		}
		return printJSON(stdout, devices)

	case "invoke":
		if len(args) != 5 {
			return errors.New("usage: stulp invoke APP_ID DEVICE_ID CAPABILITY_ID JSON_VALUE")
		}
		var value any
		if err := json.Unmarshal([]byte(args[4]), &value); err != nil {
			return fmt.Errorf("decode capability value: %w", err)
		}
		runner, err := startRunner(ctx, database, args[1], *language, *timezone)
		if err != nil {
			return err
		}
		defer runner.Close()
		if err := runner.InvokeCapability(ctx, args[2], args[3], value, map[string]any{}); err != nil {
			return err
		}
		device, err := database.Device(ctx, args[2])
		if err != nil {
			return err
		}
		return printJSON(stdout, device)

	case "flow":
		if len(args) < 4 || len(args) > 6 {
			return errors.New("usage: stulp flow APP_ID TYPE CARD_ID [ARGS_JSON] [STATE_JSON]")
		}
		flowArgs := map[string]any{}
		flowState := map[string]any{}
		if len(args) >= 5 {
			if err := json.Unmarshal([]byte(args[4]), &flowArgs); err != nil {
				return fmt.Errorf("decode flow arguments: %w", err)
			}
		}
		if len(args) == 6 {
			if err := json.Unmarshal([]byte(args[5]), &flowState); err != nil {
				return fmt.Errorf("decode flow state: %w", err)
			}
		}
		runner, err := startRunner(ctx, database, args[1], *language, *timezone)
		if err != nil {
			return err
		}
		defer runner.Close()
		value, err := runner.InvokeFlow(ctx, args[2], args[3], flowArgs, flowState)
		if err != nil {
			return err
		}
		return printJSON(stdout, value)

	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func addDevice(ctx context.Context, database *store.Store, arguments []string, stdout, stderr io.Writer) error {
	command := flag.NewFlagSet("add-device", flag.ContinueOnError)
	command.SetOutput(stderr)
	name := command.String("name", "", "device name")
	class := command.String("class", "", "device class (defaults to driver class)")
	dataJSON := command.String("data", "{}", "pairing identity data as JSON")
	settingsJSON := command.String("settings", "{}", "initial settings as JSON")
	if err := command.Parse(arguments); err != nil {
		return err
	}
	if command.NArg() != 2 || *name == "" {
		return errors.New("usage: stulp add-device --name NAME [--data JSON] APP_ID DRIVER_ID")
	}
	appID, driverID := command.Arg(0), command.Arg(1)
	app, err := database.App(ctx, appID)
	if err != nil {
		return err
	}
	appManifest, _, err := manifest.Load(app.Root)
	if err != nil {
		return err
	}
	driver, ok := appManifest.Driver(driverID)
	if !ok {
		return fmt.Errorf("driver %q does not exist in app %q", driverID, appID)
	}
	if *class == "" {
		*class = driver.Class
	}
	var data, settings map[string]any
	if err := json.Unmarshal([]byte(*dataJSON), &data); err != nil {
		return fmt.Errorf("decode device data: %w", err)
	}
	if err := json.Unmarshal([]byte(*settingsJSON), &settings); err != nil {
		return fmt.Errorf("decode device settings: %w", err)
	}
	device, err := database.AddDevice(ctx, store.Device{
		AppID: appID, DriverID: driverID, Name: *name, Class: *class,
		Data: data, Settings: settings, Capabilities: driver.Capabilities,
	})
	if err != nil {
		return err
	}
	return printJSON(stdout, device)
}

func startRunner(ctx context.Context, database *store.Store, appID, language, timezone string) (plugin.Runtime, error) {
	runner, err := plugin.NewRuntime(ctx, database, appID, plugin.Options{
		Language: language, Timezone: timezone,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return nil, err
	}
	if err := runner.Start(ctx); err != nil {
		return nil, err
	}
	return runner, nil
}

// parseLogLevel weigert een onbekend niveau in plaats van stil terug te vallen
// op info. Wie debug vraagt en info krijgt zoekt zich rot naar regels die er
// wel zijn maar niet getoond worden.
func parseLogLevel(name string) (slog.Level, error) {
	switch name {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q: use debug, info, warn or error", name)
}

func printJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func usage(writer io.Writer) {
	executable := filepath.Base(os.Args[0])
	fmt.Fprintf(writer, `%[1]s — a small home controller that runs Go plugins

Usage:
  %[1]s [--document PATH] install APP_DIRECTORY_OR_GITHUB_URL
  %[1]s [--document PATH] check-updates [APP_ID]
  %[1]s [--document PATH] update APP_ID
  %[1]s [--document PATH] uninstall APP_ID
  %[1]s [--document PATH] backup OUTPUT.zip
  %[1]s [--document PATH] restore BACKUP.zip
  %[1]s [--document PATH] apps
  %[1]s [--document PATH] devices [APP_ID]
  %[1]s [--document PATH] add-device --name NAME [--data JSON] APP_ID DRIVER_ID
  %[1]s [--document PATH] run [--once] APP_ID
  %[1]s [--document PATH] serve [--listen ADDRESS] [--token KEY] [--tls-cert FILE --tls-key FILE]
                                [--attach SOCKET] [--attach-port ADDRESS]
  %[1]s [--document PATH] attach-token APP_ID
  %[1]s [--document PATH] attach-token --rotate
  %[1]s [--document PATH] inspect APP_ID
  %[1]s [--document PATH] pair-list APP_ID DRIVER_ID
  %[1]s [--document PATH] invoke APP_ID DEVICE_ID CAPABILITY_ID JSON_VALUE
  %[1]s [--document PATH] flow APP_ID TYPE CARD_ID [ARGS_JSON] [STATE_JSON]
`, executable)
}
