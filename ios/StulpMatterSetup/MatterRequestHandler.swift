import MatterSupport
import os

final class MatterRequestHandler: MatterAddDeviceExtensionRequestHandler {
  private let logger = Logger(subsystem: "com.xinix00.stulp", category: "MatterSetup")

  override func selectWiFiNetwork(
    from wifiScanResults: [MatterAddDeviceExtensionRequestHandler.WiFiScanResult]
  ) async throws -> MatterAddDeviceExtensionRequestHandler.WiFiNetworkAssociation {
    .defaultSystemNetwork
  }

  override func selectThreadNetwork(
    from threadScanResults: [MatterAddDeviceExtensionRequestHandler.ThreadScanResult]
  ) async throws -> MatterAddDeviceExtensionRequestHandler.ThreadNetworkAssociation {
    .defaultSystemNetwork
  }

  override func validateDeviceCredential(
    _ deviceCredential: MatterAddDeviceExtensionRequestHandler.DeviceCredential
  ) async throws {
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
    do {
      try PendingMatterDevices.remove()
      let devices = try await StulpClient(connection: connection)
        .commissionMatter(onboardingPayload: onboardingPayload)
      try PendingMatterDevices.save(devices)
      logger.info("Matter commissioning added \(devices.count, privacy: .public) Stulp device(s)")
    } catch {
      logger.error(
        "Stulp Matter commissioning failed: \(error.localizedDescription, privacy: .public)")
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
