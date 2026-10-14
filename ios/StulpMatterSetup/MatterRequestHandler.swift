import MatterSupport
import os

final class MatterRequestHandler: MatterAddDeviceExtensionRequestHandler {
  private let logger = Logger(subsystem: "com.xinix00.stulp", category: "MatterSetup")

  /// Dezelfde regels gaan naar de console van de node (POST /api/stulp/log),
  /// zodat een koppeling zonder sudo-logarchief van de telefoon te volgen is.
  private func client() -> StulpClient? {
    guard let connection = try? SharedStulpConfiguration.load() else { return nil }
    return StulpClient(connection: connection, logSource: "ios-matter")
  }

  private func note(_ message: String) {
    note("info", message)
  }

  private func note(_ level: String, _ message: String) {
    logger.info("\(message, privacy: .public)")
    MatterProgress.record(message)
    client()?.trace(level, message)
  }

  override func selectWiFiNetwork(
    from wifiScanResults: [MatterAddDeviceExtensionRequestHandler.WiFiScanResult]
  ) async throws -> MatterAddDeviceExtensionRequestHandler.WiFiNetworkAssociation {
    note("wifi: \(wifiScanResults.count) netwerk(en) gezien, systeemnetwerk gekozen")
    return .defaultSystemNetwork
  }

  override func selectThreadNetwork(
    from threadScanResults: [MatterAddDeviceExtensionRequestHandler.ThreadScanResult]
  ) async throws -> MatterAddDeviceExtensionRequestHandler.ThreadNetworkAssociation {
    let names = threadScanResults.map { $0.networkName }.joined(separator: ", ")
    note("thread: \(threadScanResults.count) netwerk(en) gezien [\(names)], systeemnetwerk gekozen")
    return .defaultSystemNetwork
  }

  override func validateDeviceCredential(
    _ deviceCredential: MatterAddDeviceExtensionRequestHandler.DeviceCredential
  ) async throws {
    note("credential gevalideerd door iOS; Stulp controleert DAC en PAI zelf tijdens PASE")
    // Apples setupstroom valideert de standaard credential. Stulp controleert
    // tijdens zijn eigen PASE-sessie vervolgens zelf DAC->PAI, nonce en
    // handtekening voordat het een NOC uitgeeft. Hier voegen we bewust geen
    // half trustbeleid toe zonder een eigen PAA/DCL-store.
  }

  override func commissionDevice(
    in home: MatterAddDeviceRequest.Home?,
    onboardingPayload: String,
    commissioningID: UUID
  ) async throws {
    guard let connection = try SharedStulpConfiguration.load() else {
      throw MatterSetupError.notConfigured
    }
    note("commissionDevice: iOS draagt over, payload \(onboardingPayload.count) tekens, prefix \(onboardingPayload.prefix(3))")
    do {
      try PendingMatterDevices.remove()
      let devices = try await StulpClient(connection: connection, logSource: "ios-matter")
        .commissionMatter(onboardingPayload: onboardingPayload)
      MatterProgress.record("Stulp heeft \(devices.count) apparaat/apparaten toegevoegd")
      try PendingMatterDevices.save(devices)
      logger.info("Matter commissioning added \(devices.count, privacy: .public) Stulp device(s)")
    } catch {
      logger.error(
        "Stulp Matter commissioning failed: \(error.localizedDescription, privacy: .public)")
      MatterProgress.record("Stulp-koppeling mislukt: \(error.localizedDescription)")
      throw error
    }
  }

  override func rooms(in home: MatterAddDeviceRequest.Home?) async -> [MatterAddDeviceRequest.Room]
  {
    guard let connection = try? SharedStulpConfiguration.load() else { return [] }
    do {
      return try await StulpClient(connection: connection).groups()
        .map { MatterAddDeviceRequest.Room(displayName: $0.name) }
    } catch {
      logger.error("Could not load Stulp groups: \(error.localizedDescription, privacy: .public)")
      return []
    }
  }

  override func configureDevice(
    named name: String,
    in room: MatterAddDeviceRequest.Room?
  ) async {
    let devices = (try? PendingMatterDevices.load()) ?? []
    note("configureDevice: naam \"\(name)\", kamer \(room?.displayName ?? "-"), \(devices.count) apparaat/apparaten")
    defer { try? PendingMatterDevices.remove() }
    guard !devices.isEmpty,
      let connection = try? SharedStulpConfiguration.load()
    else { return }
    do {
      try await StulpClient(connection: connection)
        .configure(devices: devices, name: name, roomName: room?.displayName)
    } catch {
      // Het apparaat en Stulps fabric zijn dan al veilig gekoppeld. Naam
      // en groep zijn afwerking; een fout hier mag dat resultaat niet als
      // een mislukte commissioning terugrollen.
      logger.error(
        "Could not configure the new Stulp device: \(error.localizedDescription, privacy: .public)")
    }
  }
}

private enum MatterSetupError: LocalizedError {
  case notConfigured

  var errorDescription: String? {
    switch self {
    case .notConfigured:
      "Open Stulp eerst en verbind de app met je Manage-link."
    }
  }
}
