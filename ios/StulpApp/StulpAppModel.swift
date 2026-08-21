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

  func addMatterDevice() async {
    guard connection != nil else { return }
    guard MatterAddDeviceRequest.isSupported else {
      status = "Matter toevoegen wordt op dit iOS-apparaat niet ondersteund."
      return
    }
    isWorking = true
    status = "iOS opent de Matter-scanner…"
    defer { isWorking = false }
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
    } catch {
      let nsError = error as NSError
      if nsError.domain == NSCocoaErrorDomain && nsError.code == NSUserCancelledError {
        status = "Toevoegen geannuleerd."
      } else {
        status = error.localizedDescription
      }
    }
  }
}
