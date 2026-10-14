import Combine
import Foundation
import MatterSupport

@MainActor
final class StulpAppModel: ObservableObject {
  @Published var connection: StulpConnection?
  @Published var entry = ""
  @Published var status = ""
  @Published var isWorking = false

  init() {
    do {
      connection = try SharedStulpConfiguration.load()
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

  func addMatterDevice() async {
    guard connection != nil else { return }
    guard MatterAddDeviceRequest.isSupported else {
      status = "Matter toevoegen wordt op dit iOS-apparaat niet ondersteund."
      return
    }
    isWorking = true
    status = "iOS opent de Matter-scanner…"
    defer { isWorking = false }
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
      try await request.perform()
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
