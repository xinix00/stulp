import Foundation

actor StulpClient {
  private static let matterAppID = "com.stulp.matter"
  private static let matterDriverID = "matter"

  private let connection: StulpConnection
  private let session: URLSession
  private let pollIntervalNanoseconds: UInt64
  private let commissioningTimeout: Duration
  private let candidateRetryDelayNanoseconds: UInt64
  private var cookie: String?

  init(
    connection: StulpConnection,
    configuration: URLSessionConfiguration = .ephemeral,
    pollIntervalNanoseconds: UInt64 = 1_500_000_000,
    commissioningTimeout: Duration = .seconds(195),
    candidateRetryDelayNanoseconds: UInt64 = 250_000_000
  ) {
    self.connection = connection
    configuration.timeoutIntervalForRequest = 25
    configuration.timeoutIntervalForResource = 35
    configuration.httpShouldSetCookies = false
    self.session = URLSession(configuration: configuration)
    self.pollIntervalNanoseconds = pollIntervalNanoseconds
    self.commissioningTimeout = commissioningTimeout
    self.candidateRetryDelayNanoseconds = candidateRetryDelayNanoseconds
  }

  func validate() async throws -> StulpHealth {
    try await authenticate()
    return try await request("/api/stulp/health", as: StulpHealth.self)
  }

  func groups() async throws -> [StulpDeviceGroup] {
    try await request("/api/stulp/device-groups", as: [StulpDeviceGroup].self)
  }

  // MatterSupport heeft het apparaat vóór deze callback via BLE op wifi of
  // Thread gezet. Vanaf hier is de iPhone alleen nog de koerier: Stulp doet
  // zelf PASE, attestation, NOC/CASE en de inspectie van de endpoints.
  func commissionMatter(onboardingPayload: String) async throws -> [PendingMatterDevice] {
    let payload = onboardingPayload.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !payload.isEmpty else { throw StulpClientError.missingOnboardingPayload }

    let pair = try await request(
      "/api/stulp/pair",
      method: "POST",
      json: ["appId": Self.matterAppID, "driverId": Self.matterDriverID],
      as: PairSession.self
    )
    guard pair.handlers.contains("commission"), pair.handlers.contains("commission_state"),
      pair.handlers.contains("list_devices")
    else {
      try? await closePair(pair.id)
      throw StulpClientError.incompatibleMatterApp
    }

    return try await withTaskCancellationHandler {
      do {
        let devices = try await performCommissioning(pairID: pair.id, payload: payload)
        await closePairIgnoringCancellation(pair.id)
        return devices
      } catch {
        await closePairIgnoringCancellation(pair.id)
        throw error
      }
    } onCancel: {
      Task.detached { [self] in
        try? await closePair(pair.id)
      }
    }
  }

  func configure(devices: [PendingMatterDevice], name: String, roomName: String?) async throws {
    guard !devices.isEmpty else { return }
    let cleanName = name.trimmingCharacters(in: .whitespacesAndNewlines)

    // Een bridge kan meerdere echte Stulp-apparaten opleveren. Eén naam uit
    // Apple's scherm over al die endpoints uitrollen zou hun bruikbare namen
    // wissen; alleen een enkelvoudig resultaat neemt die naam daarom over.
    if devices.count == 1, !cleanName.isEmpty, cleanName != devices[0].name {
      _ = try await request(
        "/api/manager/devices/device/\(escape(devices[0].id))",
        method: "PUT",
        json: ["name": cleanName],
        as: AddedDevice.self
      )
    }

    guard let roomName, !roomName.isEmpty else { return }
    let availableGroups = try await groups()
    guard let group = availableGroups.first(where: { $0.name == roomName }) else { return }
    for device in devices {
      _ = try await request(
        "/api/stulp/devices/\(escape(device.id))/group",
        method: "PUT",
        json: ["groupId": group.id],
        as: AddedDevice.self
      )
    }
  }

  private func emit<T: Decodable>(
    pairID: String,
    event: String,
    json: [String: String],
    as type: T.Type
  ) async throws -> T {
    try await request(
      "/api/stulp/pair/\(escape(pairID))/emit/\(escape(event))",
      method: "POST",
      json: json,
      as: type
    )
  }

  private func emitData(pairID: String, event: String, json: [String: String]?) async throws -> Data
  {
    let body = try json.map { try JSONSerialization.data(withJSONObject: $0) }
    return try await requestData(
      "/api/stulp/pair/\(escape(pairID))/emit/\(escape(event))",
      method: "POST",
      body: body,
      contentType: body == nil ? nil : "application/json"
    )
  }

  private func closePair(_ pairID: String) async throws {
    _ = try await requestData("/api/stulp/pair/\(escape(pairID))", method: "DELETE")
  }

  private func performCommissioning(pairID: String, payload: String) async throws
    -> [PendingMatterDevice]
  {
    let clock = ContinuousClock()
    let deadline = clock.now.advanced(by: commissioningTimeout)
    var state = try await emit(
      pairID: pairID,
      event: "commission",
      json: ["code": payload, "address": ""],
      as: CommissionState.self
    )
    while state.running {
      try Task.checkCancellation()
      guard clock.now < deadline else { throw StulpClientError.commissioningTimedOut }
      try await Task.sleep(nanoseconds: pollIntervalNanoseconds)
      state = try await emit(
        pairID: pairID,
        event: "commission_state",
        json: [:],
        as: CommissionState.self
      )
    }
    if let warning = state.warning, !warning.isEmpty {
      throw StulpClientError.commissioningFailed(warning)
    }
    guard (state.found?.found ?? 0) > 0 else {
      throw StulpClientError.noDevicesFound
    }

    let candidates = try await emitData(pairID: pairID, event: "list_devices", json: nil)
    let objects = try candidateObjects(from: candidates)
    guard !objects.isEmpty else { throw StulpClientError.noDevicesFound }

    var devices: [PendingMatterDevice] = []
    for object in objects {
      let body = try JSONSerialization.data(withJSONObject: object)
      devices.append(try await addCandidate(body).pending)
    }
    return devices
  }

  private func addCandidate(_ body: Data) async throws -> AddedDevice {
    var firstError: Error?
    for attempt in 0..<2 {
      do {
        let added = try await requestData(
          "/api/stulp/apps/\(Self.matterAppID)/drivers/\(Self.matterDriverID)/pair/devices",
          method: "POST",
          body: body,
          contentType: "application/json"
        )
        return try decode(AddedDevice.self, from: added)
      } catch {
        if attempt == 1 || !isRetryableCandidateError(error) { throw error }
        firstError = error
        try Task.checkCancellation()
        try await Task.sleep(nanoseconds: candidateRetryDelayNanoseconds)
      }
    }
    throw firstError ?? StulpClientError.invalidResponse
  }

  private func isRetryableCandidateError(_ error: Error) -> Bool {
    if let urlError = error as? URLError {
      return urlError.code != .cancelled
    }
    if let clientError = error as? StulpClientError,
      case .server(let status, _) = clientError
    {
      return status >= 500
    }
    return false
  }

  private func closePairIgnoringCancellation(_ pairID: String) async {
    _ = await Task.detached { [self] in
      try? await closePair(pairID)
    }.value
  }

  private func authenticate() async throws {
    if cookie != nil { return }
    var request = URLRequest(url: connection.entryURL)
    request.httpMethod = "GET"
    request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
    let (data, response) = try await session.data(for: request)
    let http = try checked(response: response, data: data, notFoundIsUnauthorized: true)
    guard let setCookie = http.value(forHTTPHeaderField: "Set-Cookie"),
      let proof = setCookie.split(separator: ";", maxSplits: 1).first,
      proof.hasPrefix("stulp-session=")
    else {
      throw StulpClientError.missingSessionCookie
    }
    cookie = String(proof)
  }

  private func request<T: Decodable>(
    _ path: String,
    method: String = "GET",
    json: [String: String]? = nil,
    as type: T.Type
  ) async throws -> T {
    let body = try json.map { try JSONSerialization.data(withJSONObject: $0) }
    let data = try await requestData(
      path,
      method: method,
      body: body,
      contentType: body == nil ? nil : "application/json"
    )
    return try decode(type, from: data)
  }

  private func requestData(
    _ path: String,
    method: String = "GET",
    body: Data? = nil,
    contentType: String? = nil
  ) async throws -> Data {
    if cookie == nil { try await authenticate() }
    guard let url = URL(string: path, relativeTo: connection.baseURL)?.absoluteURL else {
      throw StulpClientError.invalidResponse
    }
    var request = URLRequest(url: url)
    request.httpMethod = method
    request.httpBody = body
    request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
    request.setValue(cookie, forHTTPHeaderField: "Cookie")
    if let contentType { request.setValue(contentType, forHTTPHeaderField: "Content-Type") }
    let (data, response) = try await session.data(for: request)
    _ = try checked(response: response, data: data)
    return data
  }

  private func checked(
    response: URLResponse,
    data: Data,
    notFoundIsUnauthorized: Bool = false
  ) throws -> HTTPURLResponse {
    guard let http = response as? HTTPURLResponse else { throw StulpClientError.invalidResponse }
    guard (200..<300).contains(http.statusCode) else {
      let server = (try? JSONDecoder().decode(ServerError.self, from: data))
      if http.statusCode == 401 || (http.statusCode == 404 && notFoundIsUnauthorized) {
        throw StulpClientError.unauthorized
      }
      throw StulpClientError.server(status: http.statusCode, message: server?.message)
    }
    return http
  }

  private func decode<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
    do { return try JSONDecoder().decode(type, from: data) } catch {
      throw StulpClientError.invalidResponse
    }
  }

  private func candidateObjects(from data: Data) throws -> [[String: Any]] {
    guard let objects = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else {
      throw StulpClientError.invalidResponse
    }
    return objects
  }

  private func escape(_ value: String) -> String {
    value.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? value
  }
}

struct StulpHealth: Decodable, Equatable, Sendable {
  let ok: Bool
  let stulpVersion: String
}

struct StulpDeviceGroup: Decodable, Equatable, Sendable {
  let id: String
  let name: String
  let parentId: String?
}

private struct PairSession: Decodable, Sendable {
  let id: String
  let handlers: [String]
}

private struct CommissionState: Decodable, Sendable {
  struct Found: Decodable, Sendable { let found: Int }
  let running: Bool
  let warning: String?
  let found: Found?
}

private struct AddedDevice: Decodable, Sendable {
  let id: String
  let name: String

  var pending: PendingMatterDevice { PendingMatterDevice(id: id, name: name) }
}

private struct ServerError: Decodable, Sendable {
  let error: String?
  let errorDescription: String?

  enum CodingKeys: String, CodingKey {
    case error
    case errorDescription = "error_description"
  }

  var message: String? { errorDescription ?? error }
}

enum StulpClientError: LocalizedError, Equatable {
  case unauthorized
  case missingSessionCookie
  case invalidResponse
  case missingOnboardingPayload
  case incompatibleMatterApp
  case noDevicesFound
  case commissioningTimedOut
  case commissioningFailed(String)
  case server(status: Int, message: String?)

  var errorDescription: String? {
    switch self {
    case .unauthorized:
      "Stulp wees deze Manage-link af. Controleer het adres en de sleutel."
    case .missingSessionCookie:
      "De server gaf geen Stulp-sessie terug. Gebruik de volledige Manage-link."
    case .invalidResponse:
      "Stulp gaf een antwoord dat deze app niet begrijpt."
    case .missingOnboardingPayload:
      "iOS gaf geen Matter-koppelcode terug."
    case .incompatibleMatterApp:
      "De draaiende Matter-app ondersteunt deze iOS-koppeling nog niet."
    case .noDevicesFound:
      "Matter is gekoppeld, maar er kwam geen apparaat terug om toe te voegen."
    case .commissioningTimedOut:
      "Stulp kreeg niet binnen 3 minuten en 15 seconden antwoord van het Matter-apparaat."
    case .commissioningFailed(let message):
      message
    case .server(let status, let message):
      message ?? "Stulp gaf HTTP \(status)."
    }
  }
}
