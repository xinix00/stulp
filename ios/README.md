# Stulp for iOS

Deze experimentele dunne companion doet één native taak die de Manage-webapp niet kan: een
fabrieksnieuw Matter-apparaat via Apples setupscherm op wifi of Thread zetten en
het daarna rechtstreeks aan Stulps eigen fabric toevoegen.

## Eenmalig instellen

1. Open `Stulp.xcodeproj` in Xcode.
2. Kies voor **Stulp** en **StulpMatterSetup** hetzelfde development team.
3. Vervang zo nodig `com.xinix00.stulp`, `group.com.xinix00.stulp` en de gedeelde
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
