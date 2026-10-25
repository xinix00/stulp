import AppIntents

/// Voor een Opdrachten-automatisering "Lader is aangesloten / losgekoppeld":
/// iOS voert hem direct uit, zonder te vragen, en de app hoeft niet te draaien.
struct ReportChargerIntent: AppIntent {
  static let title: LocalizedStringResource = "Meld lader aan Stulp"
  static let description = IntentDescription(
    "Meldt aan Stulp of dit toestel aan de lader ligt. Gebruik hem in een automatisering op 'Lader is aangesloten' en 'Lader is losgekoppeld'.")
  static let openAppWhenRun = false

  @Parameter(title: "Aan de lader", default: true)
  var charging: Bool

  static var parameterSummary: some ParameterSummary {
    Summary("Meld aan Stulp: aan de lader \(\.$charging)")
  }

  func perform() async throws -> some IntentResult {
    try await StulpPresenceReport.send(["charging": charging], source: "ios-shortcut")
    return .result()
  }
}

/// Handmatig thuis of weg melden, als aanvulling op de geofence.
struct ReportHomeIntent: AppIntent {
  static let title: LocalizedStringResource = "Meld thuis aan Stulp"
  static let description = IntentDescription("Meldt aan Stulp of dit toestel thuis is.")
  static let openAppWhenRun = false

  @Parameter(title: "Thuis", default: true)
  var home: Bool

  func perform() async throws -> some IntentResult {
    try await StulpPresenceReport.send(["home": home], source: "ios-shortcut")
    return .result()
  }
}

struct StulpShortcuts: AppShortcutsProvider {
  static var appShortcuts: [AppShortcut] {
    AppShortcut(
      intent: ReportChargerIntent(),
      phrases: ["Meld lader aan \(.applicationName)"],
      shortTitle: "Lader melden",
      systemImageName: "battery.100.bolt")
    AppShortcut(
      intent: ReportHomeIntent(),
      phrases: ["Meld thuis aan \(.applicationName)"],
      shortTitle: "Thuis melden",
      systemImageName: "house")
  }
}
