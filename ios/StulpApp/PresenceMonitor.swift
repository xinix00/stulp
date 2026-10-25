import CoreLocation
import Foundation
import os

/// Thuis en weg uit twee ringen die iOS zelf bewaakt, ook als de app niet
/// draait: binnenkomen binnen 300 m maakt thuis (vroeg genoeg om het alarm eraf
/// te halen), 150 m verlaten maakt weg. De andere twee overgangen tellen niet,
/// zodat een wiebelend GPS-signaal thuis geen weg maakt. iOS wekt de app bij
/// een overgang; de app meldt het aan Stulp, en de flows beslissen wat ermee
/// gebeurt.
@MainActor
final class PresenceMonitor: NSObject, ObservableObject, CLLocationManagerDelegate {
  static let shared = PresenceMonitor()

  @Published private(set) var authorization: CLAuthorizationStatus
  @Published private(set) var lastEvent = ""

  private let manager = CLLocationManager()
  private let logger = Logger(subsystem: "software.haas.stulp", category: "Presence")
  private var monitor: CLMonitor?
  private var listening: Task<Void, Never>?
  private var locating: CheckedContinuation<CLLocation, any Error>?

  override private init() {
    authorization = manager.authorizationStatus
    super.init()
    manager.delegate = self
    manager.desiredAccuracy = kCLLocationAccuracyHundredMeters
  }

  /// Bij elke start van de app, ook als iOS hem op de achtergrond wekt voor
  /// een ring: de monitor moet dan direct weer luisteren om het event te krijgen.
  func start() {
    guard listening == nil else { return }
    listening = Task { [weak self] in
      await self?.listen()
    }
  }

  func requestAlways() {
    switch manager.authorizationStatus {
    case .notDetermined: manager.requestWhenInUseAuthorization()
    case .authorizedWhenInUse: manager.requestAlwaysAuthorization()
    default: break
    }
  }

  /// Zet thuis op de huidige plek en herstart de ringen.
  func setHome() async throws {
    requestAlways()
    let location = try await currentLocation()
    guard var settings = StulpPresenceSettings.load() else { throw StulpPresenceError.noPhone }
    settings.latitude = location.coordinate.latitude
    settings.longitude = location.coordinate.longitude
    try settings.save()
    try await configure(settings)
  }

  func clearHome() async {
    if let monitor {
      for id in await monitor.identifiers { await monitor.remove(id) }
    }
  }

  private func configure(_ settings: StulpPresenceSettings) async throws {
    guard let lat = settings.latitude, let lon = settings.longitude else { return }
    let monitor = try await ensureMonitor()
    let center = CLLocationCoordinate2D(latitude: lat, longitude: lon)
    for id in await monitor.identifiers { await monitor.remove(id) }
    await monitor.add(
      CLMonitor.CircularGeographicCondition(center: center, radius: settings.arriveRadius),
      identifier: "arrive", assuming: .unsatisfied)
    await monitor.add(
      CLMonitor.CircularGeographicCondition(center: center, radius: settings.leaveRadius),
      identifier: "leave", assuming: .satisfied)
    note("ringen gezet: thuis binnen \(Int(settings.arriveRadius)) m, weg buiten \(Int(settings.leaveRadius)) m")
  }

  private func ensureMonitor() async throws -> CLMonitor {
    if let monitor { return monitor }
    let created = await CLMonitor("stulp-presence")
    monitor = created
    return created
  }

  private func listen() async {
    do {
      let monitor = try await ensureMonitor()
      if await monitor.identifiers.isEmpty, let settings = StulpPresenceSettings.load() {
        try await configure(settings)
      }
      for try await event in await monitor.events {
        switch (event.identifier, event.state) {
        case ("arrive", .satisfied): await report(["home": true])
        case ("leave", .unsatisfied): await report(["home": false])
        default: continue
        }
      }
    } catch {
      note("geofence gestopt: \(error.localizedDescription)")
    }
    listening = nil
  }

  private func report(_ values: [String: Bool]) async {
    let text = values.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: " ")
    lastEvent = "\(Date().formatted(date: .omitted, time: .shortened)) \(text)"
    do {
      try await StulpPresenceReport.send(values, source: "ios-presence")
    } catch {
      note("melden mislukt (\(text)): \(error.localizedDescription)")
    }
  }

  private func currentLocation() async throws -> CLLocation {
    try await withCheckedThrowingContinuation { continuation in
      locating = continuation
      manager.requestLocation()
    }
  }

  private func note(_ text: String) {
    logger.info("\(text, privacy: .public)")
  }

  nonisolated func locationManagerDidChangeAuthorization(_ manager: CLLocationManager) {
    let status = manager.authorizationStatus
    Task { @MainActor in
      self.authorization = status
      // Na "tijdens gebruik" direct om "altijd" vragen: zonder dat wekt iOS
      // de app niet voor de ringen.
      if status == .authorizedWhenInUse { self.manager.requestAlwaysAuthorization() }
    }
  }

  nonisolated func locationManager(_ manager: CLLocationManager, didUpdateLocations locations: [CLLocation]) {
    guard let location = locations.last else { return }
    Task { @MainActor in
      self.locating?.resume(returning: location)
      self.locating = nil
    }
  }

  nonisolated func locationManager(_ manager: CLLocationManager, didFailWithError error: any Error) {
    Task { @MainActor in
      self.locating?.resume(throwing: error)
      self.locating = nil
    }
  }
}
