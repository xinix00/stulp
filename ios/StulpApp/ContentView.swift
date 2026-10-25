import CoreLocation
import SwiftUI

/// Zonder verbinding de eenmalige koppeling; daarna twee tabbladen: Thuis is
/// Manage met al je apps, Instellingen is wat deze telefoon voor Stulp doet.
struct ContentView: View {
  @ObservedObject var model: StulpAppModel
  @ObservedObject private var push = PushCenter.shared
  @State private var tab = ContentView.initialTab

  var body: some View {
    if let connection = model.connection {
      TabView(selection: $tab) {
        HomeView(model: model, connection: connection)
          .tabItem { Label("Thuis", systemImage: "house.fill") }
          .tag("home")
        SettingsView(model: model, connection: connection)
          .tabItem { Label("Instellingen", systemImage: "gearshape.fill") }
          .tag("settings")
      }
      .onChange(of: push.openDevice) { _, id in
        if id != nil { tab = "home" }
      }
    } else {
      OnboardingView(model: model)
    }
  }
}

extension ContentView {
  static var initialTab: String {
    #if DEBUG
      // `-stulpTab settings` voor schermafbeeldingen in de simulator.
      return UserDefaults.standard.string(forKey: "stulpTab") ?? "home"
    #else
      return "home"
    #endif
  }
}

// MARK: - Thuis

private struct HomeView: View {
  @ObservedObject var model: StulpAppModel
  @ObservedObject private var push = PushCenter.shared
  let connection: StulpConnection
  @State private var adding = false
  @State private var reloadToken = 0

  var body: some View {
    // Manage heeft zijn eigen kop; daar komt geen iOS-balk boven. De pagina
    // loopt door tot onder de statusbalk in Manage's eigen achtergrond, en
    // Matter toevoegen is één zwevende knop boven de tabbalk.
    StulpWebView(connection: connection, reloadToken: $reloadToken, openDevice: $push.openDevice)
      .background(Self.manageBackground.ignoresSafeArea())
      .overlay(alignment: .bottomTrailing) {
        Button {
          adding = true
        } label: {
          Image(systemName: "plus")
            .font(.title2.weight(.semibold))
            .foregroundStyle(.white)
            .frame(width: 56, height: 56)
            .background(Color.accentColor, in: Circle())
            .shadow(color: .black.opacity(0.4), radius: 8, y: 4)
        }
        .padding(.trailing, 20)
        .padding(.bottom, 16)
        .accessibilityLabel("Matter-apparaat toevoegen")
        .accessibilityIdentifier("add-device")
      }
      .sheet(isPresented: $adding) {
        AddDeviceView(model: model)
          .presentationDetents([.medium, .large])
      }
  }

  /// Manage's `--bg`.
  static let manageBackground = Color(red: 0x0B / 255, green: 0x0B / 255, blue: 0x0C / 255)
}

// MARK: - Apparaat toevoegen

private struct AddDeviceView: View {
  @ObservedObject var model: StulpAppModel
  @Environment(\.dismiss) private var dismiss

  var body: some View {
    NavigationStack {
      ScrollView {
        VStack(alignment: .leading, spacing: 20) {
          Label {
            VStack(alignment: .leading, spacing: 4) {
              Text("Matter-apparaat").font(.headline)
              Text(
                "Scan de QR-code op het apparaat. iOS regelt Bluetooth en het netwerk, daarna voegt Stulp het rechtstreeks aan je eigen controller toe."
              )
              .font(.subheadline)
              .foregroundStyle(.secondary)
            }
          } icon: {
            Image(systemName: "qrcode.viewfinder")
              .font(.title)
              .foregroundStyle(Color.accentColor)
          }

          Button {
            Task { await model.addMatterDevice() }
          } label: {
            Label(model.isWorking ? "Bezig…" : "Scan en voeg toe", systemImage: "plus.circle.fill")
              .frame(maxWidth: .infinity)
              .padding(.vertical, 6)
          }
          .buttonStyle(.borderedProminent)
          .disabled(model.isWorking)
          .accessibilityIdentifier("add-matter-device")

          if model.isWorking || !model.steps.isEmpty {
            MatterProgressView(model: model)
          }

          if !model.status.isEmpty {
            Text(model.status)
              .font(.footnote)
              .foregroundStyle(.secondary)
          }

          Text(
            "Andere apparaten, zoals een camera, zonwering of warmtepomp, voeg je toe via Apparaat toevoegen in Thuis."
          )
          .font(.footnote)
          .foregroundStyle(.secondary)
        }
        .padding(20)
      }
      .navigationTitle("Apparaat toevoegen")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar {
        ToolbarItem(placement: .confirmationAction) {
          Button("Klaar") { dismiss() }
        }
      }
    }
  }
}

/// De mijlpalen van de lopende koppeling, met de tijd sinds de start, en de
/// waarschuwing plus annuleerknop zodra Apple's fase blijft hangen.
private struct MatterProgressView: View {
  @ObservedObject var model: StulpAppModel

  var body: some View {
    VStack(alignment: .leading, spacing: 8) {
      let start = model.steps.first?.at
      ForEach(Array(model.steps.suffix(8).enumerated()), id: \.offset) { _, step in
        HStack(alignment: .top, spacing: 8) {
          Text(start.map { String(format: "%3.0fs", step.at.timeIntervalSince($0)) } ?? "")
            .font(.caption.monospacedDigit())
            .foregroundStyle(.secondary)
          Text(step.text)
            .font(.caption)
        }
      }
      if model.isWorking && model.steps.isEmpty {
        Text("Wachten op iOS: QR-code, Bluetooth en netwerk…")
          .font(.caption)
          .foregroundStyle(.secondary)
      }
      if !model.hint.isEmpty {
        Label(model.hint, systemImage: "exclamationmark.triangle.fill")
          .font(.footnote)
          .foregroundStyle(.orange)
      }
      if model.isWorking {
        Button("Annuleer", role: .cancel) { model.cancelAdding() }
          .font(.subheadline)
      }
    }
    .padding(12)
    .frame(maxWidth: .infinity, alignment: .leading)
    .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 12))
  }
}

// MARK: - Instellingen

private struct SettingsView: View {
  @ObservedObject var model: StulpAppModel
  @ObservedObject private var presence = PresenceMonitor.shared
  @ObservedObject private var push = PushCenter.shared
  let connection: StulpConnection
  @State private var newName = ""
  @State private var confirmDisconnect = false
  @Environment(\.openURL) private var openURL

  var body: some View {
    NavigationStack {
      Form {
        Section("Verbinding") {
          LabeledContent("Stulp", value: connection.baseURL.host ?? "")
          Button {
            openURL(connection.entryURL)
          } label: {
            Label("Open in Safari", systemImage: "safari")
          }
          Button("Verbinding wijzigen", role: .destructive) { confirmDisconnect = true }
        }

        Section {
          if let presence = model.presence {
            LabeledContent("Dit toestel", value: presence.phoneName)
          }
          Menu {
            ForEach(model.phones) { phone in
              Button(phone.name) { model.choosePhone(phone) }
            }
          } label: {
            Label(
              model.presence == nil ? "Kies een bestaand toestel" : "Ander toestel kiezen",
              systemImage: "iphone")
          }
          .disabled(model.phones.isEmpty)
          HStack {
            TextField("Bijvoorbeeld iPhone van Daphne", text: $newName)
              .textInputAutocapitalization(.words)
            Button("Registreer") {
              Task {
                await model.registerPhone(name: newName)
                newName = ""
              }
            }
            .disabled(newName.trimmingCharacters(in: .whitespaces).isEmpty || model.isWorking)
          }
        } header: {
          Text("Dit toestel in Stulp")
        } footer: {
          Text(
            "Elke telefoon is een eigen apparaat in Stulp. Registreer hier een nieuw toestel, of kies er een die al bestaat."
          )
        }

        Section {
          permissionRow
          Button {
            Task { await model.setHomeHere() }
          } label: {
            Label(
              model.presence?.latitude == nil ? "Stel thuis in op deze plek" : "Zet thuis opnieuw op deze plek",
              systemImage: "mappin.and.ellipse")
          }
          .disabled(model.presence == nil || model.isWorking)
          if !presence.lastEvent.isEmpty {
            LabeledContent("Laatst gemeld", value: presence.lastEvent)
          }
          HStack {
            Button("Meld thuis") { Task { await model.report(home: true) } }
            Spacer()
            Button("Meld weg") { Task { await model.report(home: false) } }
          }
          .buttonStyle(.borderless)
          .disabled(model.presence == nil)
        } header: {
          Text("Thuis en weg")
        } footer: {
          Text(
            "Binnen 300 m meldt thuis, zodat het alarm eraf kan voordat je bij de deur bent; 150 m verlaten meldt weg. iOS doet dat ook als de app dicht is."
          )
        }

        Section {
          Button {
            if let url = URL(string: "shortcuts://") { openURL(url) }
          } label: {
            Label("Open Opdrachten", systemImage: "square.2.layers.3d")
          }
        } header: {
          Text("Aan de lader")
        } footer: {
          Text(
            "Maak bij Automatisering twee automatiseringen op Lader: \"Is aangesloten\" en \"Is losgekoppeld\", met de actie Meld lader aan Stulp, aan of uit. Zet ze op Direct uitvoeren."
          )
        }

        Section {
          switch push.status {
          case .authorized, .provisional, .ephemeral:
            Label(
              push.registered ? "Meldingen: aan" : "Meldingen: toegestaan, nog niet bij Stulp",
              systemImage: push.registered ? "checkmark.circle.fill" : "bell.badge")
              .foregroundStyle(push.registered ? .green : .orange)
          case .denied:
            Button {
              if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
            } label: {
              Label("Zet meldingen aan in Instellingen", systemImage: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
            }
          default:
            Button {
              Task { await push.request() }
            } label: {
              Label("Sta meldingen toe", systemImage: "bell.badge")
            }
            .disabled(model.presence == nil)
          }
          if !push.lastError.isEmpty {
            Text(push.lastError).font(.footnote).foregroundStyle(.secondary)
          }
        } header: {
          Text("Meldingen")
        } footer: {
          Text(
            "Stulp stuurt meldingen rechtstreeks naar deze app: gewoon, dringend (door Focus heen) of kritiek, met camerabeeld. Tik op een melding met camerabeeld en Thuis opent die camera. Kies eerst welk toestel dit is."
          )
        }

        if !model.status.isEmpty {
          Section {
            Text(model.status).font(.footnote).foregroundStyle(.secondary)
          }
        }

        Section("Over") {
          LabeledContent("Versie", value: Self.version)
        }
      }
      .navigationTitle("Instellingen")
      .task { await model.loadPhones() }
      .refreshable { await model.loadPhones() }
      .confirmationDialog(
        "Verbinding met Stulp verwijderen?", isPresented: $confirmDisconnect, titleVisibility: .visible
      ) {
        Button("Verwijderen", role: .destructive) { model.disconnect() }
      } message: {
        Text("De app vergeet de Manage-link, het gekozen toestel en de thuislocatie.")
      }
    }
  }

  /// Zonder "Altijd" wekt iOS de app niet voor thuis/weg; dat moet zichtbaar zijn.
  @ViewBuilder private var permissionRow: some View {
    switch presence.authorization {
    case .authorizedAlways:
      Label("Locatie: altijd", systemImage: "checkmark.circle.fill")
        .foregroundStyle(.green)
    case .notDetermined:
      Button {
        presence.requestAlways()
      } label: {
        Label("Locatie toestaan", systemImage: "location.circle")
      }
    default:
      Button {
        if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
      } label: {
        Label("Zet locatie op Altijd in Instellingen", systemImage: "exclamationmark.triangle.fill")
          .foregroundStyle(.orange)
      }
    }
  }

  static var version: String {
    let info = Bundle.main.infoDictionary
    let short = info?["CFBundleShortVersionString"] as? String ?? "?"
    let build = info?["CFBundleVersion"] as? String ?? "?"
    return "\(short) (\(build))"
  }
}

// MARK: - Eerste keer

private struct OnboardingView: View {
  @ObservedObject var model: StulpAppModel

  var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 24) {
        VStack(alignment: .leading, spacing: 8) {
          Image(systemName: "house.and.flag.fill")
            .font(.system(size: 44))
            .foregroundStyle(Color.accentColor)
          Text("Je huis, op je telefoon.")
            .font(.title2.bold())
          Text(
            "Stulp toont je huis, voegt Matter-apparaten toe, en meldt thuis, weg en aan de lader aan je eigen controller."
          )
          .foregroundStyle(.secondary)
        }

        VStack(alignment: .leading, spacing: 16) {
          Text("Verbind één keer").font(.headline)
          Text(
            "Plak de volledige Manage-link, inclusief de toegangssleutel. De app bewaart hem alleen in de iOS-sleutelhanger."
          )
          .font(.subheadline)
          .foregroundStyle(.secondary)
          TextField("https://stulp.local/toegangssleutel", text: $model.entry)
            .textContentType(.URL)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
            .keyboardType(.URL)
            .submitLabel(.go)
            .onSubmit { Task { await model.connect() } }
            .padding(12)
            .background(Color(.tertiarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 12))
            .accessibilityIdentifier("manage-url")
          Button {
            Task { await model.connect() }
          } label: {
            Label(model.isWorking ? "Controleren…" : "Verbind met Stulp", systemImage: "link")
              .frame(maxWidth: .infinity)
              .padding(.vertical, 6)
          }
          .buttonStyle(.borderedProminent)
          .disabled(
            model.isWorking || model.entry.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
        }
        .padding(20)
        .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 20))

        if !model.status.isEmpty {
          Text(model.status)
            .font(.footnote)
            .foregroundStyle(.secondary)
            .accessibilityIdentifier("status")
        }
      }
      .padding(24)
    }
    .background(Color(.systemGroupedBackground))
  }
}
