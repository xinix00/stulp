# Stulp for iOS

Deze experimentele dunne companion doet één native taak die de Manage-webapp niet kan: een
fabrieksnieuw Matter-apparaat via Apples setupscherm op wifi of Thread zetten en
het daarna rechtstreeks aan Stulps eigen fabric toevoegen.

## Eenmalig instellen

1. Open `Stulp.xcodeproj` in Xcode.
2. Kies voor **Stulp** en **StulpMatterSetup** hetzelfde development team.
3. Vervang zo nodig `software.haas.stulp`, `group.software.haas.stulp` en de gedeelde
   keychain-groep in `project.yml` door identifiers van dat team en draai daarna
   opnieuw `xcodegen generate`. De Info.plists en entitlements worden daaruit
   gegenereerd.
4. Draai de app op een fysieke iPhone of iPad met iOS 17 of nieuwer.
5. Plak één keer de volledige Stulp Manage-link. De toegangssleutel komt in een
   gedeelde, device-only Keychain-entry en wordt niet gelogd of in UserDefaults
   gezet.

Daarna opent **Matter-apparaat toevoegen** Apples eigen QR- en setupscherm. iOS
doet de tijdelijke BLE- en netwerkbootstrap; de Matter Setup Extension geeft de
resulterende onboardingcode aan de gewone Stulp pair-API. Stulp voert zelf PASE,
attestation, fabric/NOC, CASE en endpointinspectie uit. Bij een bridge worden alle
gevonden endpoints toegevoegd; Apples ene gekozen naam wordt alleen gebruikt
wanneer er ook echt één Stulp-device uit kwam.

Voor lokaal HTTP staat alleen `NSAllowsLocalNetworking` aan; er is geen brede ATS-
uitzondering. Gebruik buiten een vertrouwd LAN altijd HTTPS met een certificaat
dat de iPhone vertrouwt. Stulp moet vanaf de telefoon bereikbaar zijn en met een
toegangssleutel draaien.

Een Thread-apparaat werkt alleen wanneer iOS toegang heeft tot het gekozen
default Thread-netwerk én Stulp dat netwerk via een permanente border router
kan bereiken. Dat is doorgaans een Apple TV of HomePod; een andere border
router werkt alleen als iOS de credentials voor hetzelfde Thread-netwerk kent.
De iPhone is de commissioner, niet de blijvende router.

## Op je eigen iPhone zetten

Signing staat op automatisch met team `QVY3KR44QS` in `project.yml`; de Matter-
extensie heeft de entitlement *Matter Allow Setup Payload* omdat zij de
onboardingcode rechtstreeks ontvangt. Met het Apple-account van dat team in
Xcode ingelogd en de iPhone met een kabel aangesloten, de Mac vertrouwd en
Ontwikkelaarsmodus aan (Instellingen, Privacy en beveiliging):

```sh
cd ios
./build-ipa.sh install
```

Dat genereert het project, bouwt eenmaal tegen de iPhone zodat Xcode hem in
het profiel registreert, archiveert met development-signing, exporteert
`build/Stulp.ipa` volgens `ExportOptions.plist` en installeert met `devicectl`.
Xcode registreert bij de eerste keer zelf de App IDs en de app group. Een development-build blijft zeven dagen geldig op een gratis account
en een jaar op een betaald account; daarna gewoon opnieuw draaien.

## TestFlight

```sh
cd ios
./testflight.sh export   # ondertekenen voor App Store Connect, zonder te uploaden
./testflight.sh          # archiveren en uploaden
```

Net als de andere haas.software-apps: automatische signing met het Xcode-account
van team `QVY3KR44QS`, geen App Store Connect-sleutel, en het buildnummer is de
tijd (`YYYYMMDD.HHMM`) voor app en extensie tegelijk. Het app-record met bundle-id
`software.haas.stulp` moet in App Store Connect bestaan.

## Bouwen en testen

Het projectbestand wordt uit `project.yml` gegenereerd:

```sh
cd ios
xcodegen generate
swift test --scratch-path /tmp/stulp-swift-build
xcodebuild -project Stulp.xcodeproj -scheme Stulp \
  -destination 'platform=iOS Simulator,name=iPhone 17 Pro' test \
  CODE_SIGNING_ALLOWED=NO
```

De simulatortests dekken URL-/sleutelparsing en de hele HTTP-keten: cookie-login,
pair-sessie, start-plus-poll, kandidaten toevoegen en de sessie ook bij een fout
sluiten. QR, BLE, wifi/Thread-provisioning en echte commissioning zijn Apple-
framework- en hardwaregrenzen en moeten op een fysieke iPhone met echte Matter-
apparaten worden gevalideerd.
