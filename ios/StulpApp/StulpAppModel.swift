import Combine
import Foundation
import MatterSupport

@MainActor
final class StulpAppModel: ObservableObject {
  @Published var connection: StulpConnection?
  @Published var entry = ""
  @Published var status = ""
  @Published var isWorking = false
  /// Mijlpalen van de lopende koppeling, uit de gedeelde app-opslag.
  @Published var steps: [MatterProgressStep] = []
  /// Waarschuwing zodra de overdracht aan Stulp na de netwerkstap uitblijft.
  @Published var hint = ""
  private var adding: Task<Void, any Error>?
  /// Na de netwerkstap volgt de overdracht normaal binnen seconden; daarna
  /// komt hij in de praktijk vrijwel nooit meer (14-10: vijf minuten wachten
  /// tot Apple's eigen time-out, en de tweede poging slaagde meteen).
  static let stallSeconds: TimeInterval = 40

  /// Telefoon-apparaten in Stulp, om dit toestel aan te wijzen.
  @Published var phones: [StulpPhone] = []
  @Published var presence: StulpPresenceSettings? = StulpPresenceSettings.load()

  init() {
    do {
      connection = try SharedStulpConfiguration.load()
    } catch {
      status = error.localizedDescription
    }
    #if DEBUG
      // Alleen voor de simulator: `-stulpEntry <Manage-link>` verbindt meteen,
      // zodat het ontwerp met echte gegevens te bekijken is. Release negeert het.
      if connection == nil, let entry = UserDefaults.standard.string(forKey: "stulpEntry") {
        self.entry = entry
        Task { await self.connect() }
      }
    #endif
  }

  func loadPhones() async {
    guard let connection else { return }
    do {
      phones = try await StulpClient(connection: connection).phones()
    } catch {
      status = error.localizedDescription
    }
  }

  /// Registreert deze app als eigen telefoon-apparaat in Stulp en kiest hem.
  func registerPhone(name: String) async {
    guard let connection else { return }
    let name = name.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !name.isEmpty else {
      status = "Geef dit toestel een naam."
      return
    }
    isWorking = true
    defer { isWorking = false }
    do {
      let phone = try await StulpClient(connection: connection)
        .registerPhone(name: name, installation: StulpInstallation.id)
      choosePhone(phone)
      await loadPhones()
    } catch {
      status = error.localizedDescription
    }
  }

  func choosePhone(_ phone: StulpPhone) {
    var settings = presence ?? StulpPresenceSettings(phoneId: phone.id, phoneName: phone.name)
    settings.phoneId = phone.id
    settings.phoneName = phone.name
    do {
      try settings.save()
      presence = settings
      status = "Dit toestel is \(phone.name) in Stulp."
    } catch {
      status = error.localizedDescription
    }
  }

  func setHomeHere() async {
    isWorking = true
    defer { isWorking = false }
    do {
      try await PresenceMonitor.shared.setHome()
      presence = StulpPresenceSettings.load()
      status = "Thuis staat op deze plek. Thuis binnen 300 m, weg buiten 150 m."
    } catch {
      status = error.localizedDescription
    }
  }

  func report(home: Bool) async {
    do {
      try await StulpPresenceReport.send(["home": home], source: "ios-app")
      status = home ? "Thuis gemeld." : "Weg gemeld."
    } catch {
      status = error.localizedDescription
    }
  }

  func connect() async {
    isWorking = true
    defer { isWorking = false }
    do {
      let candidate = try StulpConnection.parse(entry)
      let health = try await StulpClient(connection: candidate).validate()
      guard health.ok else { throw StulpClientError.invalidResponse }
      try SharedStulpConfiguration.save(candidate)
      connection = candidate
      entry = ""
      status = "Verbonden met Stulp \(health.stulpVersion)."
    } catch {
      status = error.localizedDescription
    }
  }

  func disconnect() {
    do {
      try SharedStulpConfiguration.remove()
      try PendingMatterDevices.remove()
      StulpPresenceSettings.remove()
      presence = nil
      phones = []
      connection = nil
      status = "Verbinding verwijderd."
    } catch {
      status = error.localizedDescription
    }
  }

  /// Domein, code en de hele keten van onderliggende fouten: MatterSupport
  /// meldt zelf alleen "fout 1", de reden zit in de userInfo daaronder.
  nonisolated static func describe(_ error: NSError, depth: Int = 0) -> String {
    var text = "\(error.domain) \(error.code) \(error.localizedDescription)"
    let info = error.userInfo.filter { $0.key != NSUnderlyingErrorKey && $0.key != "NSMultipleUnderlyingErrorsKey" }
    if !info.isEmpty {
      text += " userInfo=" + info.map { "\($0.key)=\($0.value)" }.sorted().joined(separator: "; ")
    }
    var underlying: [NSError] = []
    if let one = error.userInfo[NSUnderlyingErrorKey] as? NSError { underlying.append(one) }
    if let many = error.userInfo["NSMultipleUnderlyingErrorsKey"] as? [NSError] { underlying += many }
    if depth < 4 {
      for u in underlying { text += " <- " + describe(u, depth: depth + 1) }
    }
    return String(text.prefix(900))
  }

  func cancelAdding() {
    adding?.cancel()
    hint = ""
    status = "Toevoegen geannuleerd. Probeer het meteen opnieuw; dat slaagt meestal."
  }

  /// Leest de mijlpalen van de extensie en zet de waarschuwing als de
  /// overdracht aan Stulp te lang uitblijft.
  func refreshProgress() {
    steps = MatterProgress.load()
    guard isWorking, let last = steps.last else { return }
    let handedOver = steps.contains { $0.text.hasPrefix("commissionDevice") }
    let networkChosen = steps.contains { $0.text.hasPrefix("thread:") || $0.text.hasPrefix("wifi:") }
    let waited = Date().timeIntervalSince(last.at)
    if networkChosen && !handedOver && waited >= Self.stallSeconds {
      hint =
        "Al \(Int(waited)) s geen overdracht aan Stulp na de netwerkstap. iOS krijgt het apparaat dan meestal niet meer te pakken en wacht tot zijn eigen time-out van vijf minuten. Annuleer en probeer het meteen opnieuw."
    } else {
      hint = ""
    }
  }

  func addMatterDevice() async {
    guard connection != nil else { return }
    guard MatterAddDeviceRequest.isSupported else {
      status = "Matter toevoegen wordt op dit iOS-apparaat niet ondersteund."
      return
    }
    isWorking = true
    status = "iOS opent de Matter-scanner…"
    hint = ""
    MatterProgress.clear()
    steps = []
    let poll = Task { [weak self] in
      while !Task.isCancelled {
        try? await Task.sleep(nanoseconds: 1_000_000_000)
        await MainActor.run { self?.refreshProgress() }
      }
    }
    defer {
      poll.cancel()
      isWorking = false
      refreshProgress()
      hint = ""
    }
    let trace = connection.map { StulpClient(connection: $0, logSource: "ios-app") }
    trace?.trace("Matter toevoegen gestart vanuit de app")
    do {
      let home = MatterAddDeviceRequest.Home(displayName: "Thuis")
      let topology = MatterAddDeviceRequest.Topology(ecosystemName: "Stulp", homes: [home])
      let request = MatterAddDeviceRequest(
        topology: topology,
        showing: .allDevices,
        shouldScanNetworks: true
      )
      let work = Task { try await request.perform() }
      adding = work
      defer { adding = nil }
      try await work.value
      status = "Het apparaat is rechtstreeks aan Stulp toegevoegd."
      trace?.trace("Matter toevoegen afgerond door iOS")
    } catch {
      let nsError = error as NSError
      if nsError.domain == NSCocoaErrorDomain && nsError.code == NSUserCancelledError {
        status = "Toevoegen geannuleerd."
        trace?.trace("warn", "Matter toevoegen geannuleerd")
      } else {
        status = error.localizedDescription
        trace?.trace("error", "Matter toevoegen mislukt: \(Self.describe(nsError))")
      }
    }
  }
}
