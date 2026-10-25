import Foundation

actor StulpClient {
  private static let matterAppID = "com.stulp.matter"
  private static let matterDriverID = "matter"

  private let connection: StulpConnection
  private let session: URLSession
  private let pollIntervalNanoseconds: UInt64
  private let commissioningTimeout: Duration
  private let candidateRetryDelayNanoseconds: UInt64
  private let logSource: String
  private var cookie: String?

  init(
    connection: StulpConnection,
    configuration: URLSessionConfiguration = .ephemeral,
    pollIntervalNanoseconds: UInt64 = 1_500_000_000,
    commissioningTimeout: Duration = .seconds(195),
    candidateRetryDelayNanoseconds: UInt64 = 250_000_000,
    logSource: String = "ios"
  ) {
    self.connection = connection
    self.logSource = logSource
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

  /// Eén regel op de console van de controller, naast de plugin- en
  /// requestregels. Stulp slaat niets op; een fout hier is nooit een reden om
  /// een koppeling af te breken, dus de aanroeper negeert hem.
  func log(_ level: String, _ message: String) async throws {
    _ = try await requestData(
      "/api/stulp/log",
      method: "POST",
      body: try JSONSerialization.data(withJSONObject: [
        "source": logSource, "level": level, "message": String(message.prefix(1000)),
      ]),
      contentType: "application/json"
    )
  }

  /// Fire-and-forget variant voor onderweg in een koppeling.
  nonisolated func trace(_ message: String) {
    trace("info", message)
  }

  nonisolated func trace(_ level: String, _ message: String) {
    Task { try? await self.log(level, message) }
  }

  /// De telefoon-apparaten van de Meldingen-app, om dit toestel aan te wijzen.
  func phones() async throws -> [StulpPhone] {
    let data = try await requestData("/api/manager/devices/device")
    let all = try JSONDecoder().decode([String: StulpPhone].self, from: data)
    return all.values
      .filter { $0.appId == "com.stulp.notify" }
      .sorted { $0.name.localizedCompare($1.name) == .orderedAscending }
  }

  /// Maakt voor deze app een eigen telefoon-apparaat in Stulp, zonder
  /// browser-pushabonnement. `installation` is per installatie vast, zodat
  /// opnieuw registreren hetzelfde apparaat teruggeeft.
  func registerPhone(name: String, installation: String) async throws -> StulpPhone {
    let body = try JSONSerialization.data(withJSONObject: [
      "name": name,
      "data": ["kind": "app", "installation": installation],
    ])
    let data = try await requestData(
      "/api/stulp/apps/com.stulp.notify/drivers/phone/pair/devices",
      method: "POST",
      body: body,
      contentType: "application/json"
    )
    return try JSONDecoder().decode(StulpPhone.self, from: data)
  }

  /// Meldt wat de telefoon weet (lader, thuis) op zijn apparaat.
  func report(deviceId: String, values: [String: Bool]) async throws {
    let body = try JSONSerialization.data(withJSONObject: [
      "deviceId": deviceId, "values": values,
    ])
    _ = try await requestData(
      "/api/stulp/apps/com.stulp.notify/api/report",
      method: "POST",
      body: body,
      contentType: "application/json"
    )
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
    trace("pair \(pair.id) geopend, payload \(payload.count) tekens")

    return try await withTaskCancellationHandler {
      do {
        let devices = try await performCommissioning(pairID: pair.id, payload: payload)
        trace("pair \(pair.id) klaar: \(devices.count) apparaat/apparaten toegevoegd")
        await closePairIgnoringCancellation(pair.id)
        return devices
      } catch {
        trace("error", "pair \(pair.id) mislukt: \(error.localizedDescription)")
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
    // Een enkel verzoek kan onderweg verloren gaan terwijl Stulp gewoon
    // doorwerkt (14-10: een status-poll midden in PASE kreeg nooit antwoord).
    // "commission" geeft bij een lopende sessie de lopende staat terug, dus
    // herhalen is veilig; afbreken zou een geslaagde koppeling weggooien.
    var state = try await emitRetrying(
      pairID: pairID,
      event: "commission",
      json: ["code": payload, "address": ""],
      deadline: deadline,
      clock: clock
    )
    trace("commission gestuurd, running=\(state.running)")
    while state.running {
      try Task.checkCancellation()
      guard clock.now < deadline else {
        trace("warn", "commission_state nog running na \(Int(commissioningTimeout.components.seconds)) s")
        throw StulpClientError.commissioningTimedOut
      }
      try await Task.sleep(nanoseconds: pollIntervalNanoseconds)
      state = try await emitRetrying(
        pairID: pairID,
        event: "commission_state",
        json: [:],
        deadline: deadline,
        clock: clock
      )
    }
    trace("commission klaar: found=\(state.found ?? 0) warning=\(state.warning ?? "-")")
    if let warning = state.warning, !warning.isEmpty {
      throw StulpClientError.commissioningFailed(warning)
    }
    guard (state.found ?? 0) > 0 else {
      throw StulpClientError.noDevicesFound
    }

    let candidates = try await emitData(pairID: pairID, event: "list_devices", json: nil)
    let objects = try candidateObjects(from: candidates)
    trace("list_devices: \(objects.count) kandidaat/kandidaten")
    guard !objects.isEmpty else { throw StulpClientError.noDevicesFound }

    var devices: [PendingMatterDevice] = []
    for object in objects {
      let body = try JSONSerialization.data(withJSONObject: object)
      devices.append(try await addCandidate(body).pending)
    }
    return devices
  }

  private func emitRetrying(
    pairID: String,
    event: String,
    json: [String: String],
    deadline: ContinuousClock.Instant,
    clock: ContinuousClock
  ) async throws -> CommissionState {
    var attempt = 0
    while true {
      do {
        return try await emit(pairID: pairID, event: event, json: json, as: CommissionState.self)
      } catch let error as URLError where error.code == .timedOut || error.code == .networkConnectionLost {
        attempt += 1
        trace("warn", "\(event) zonder antwoord (poging \(attempt)): \(error.localizedDescription)")
        guard attempt < 4, clock.now < deadline else { throw error }
        try Task.checkCancellation()
      }
    }
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
        let device = try decode(AddedDevice.self, from: added)
        trace("toegevoegd: \(device.name) (\(device.id))")
        return device
      } catch {
        trace("warn", "kandidaat toevoegen mislukt (poging \(attempt + 1)): \(error.localizedDescription)")
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

struct StulpPhone: Decodable, Equatable, Sendable, Identifiable {
  let id: String
  let name: String
  let appId: String
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
  let running: Bool
  let warning: String?
  /// Stulp zet `found` bij succes op het aantal gevonden apparaten. Een ouder
  /// voorstel had hier een object; beide vormen worden gelezen.
  let found: Int?

  private enum CodingKeys: String, CodingKey { case running, warning, found }
  private struct Legacy: Decodable { let found: Int }

  init(from decoder: Decoder) throws {
    let c = try decoder.container(keyedBy: CodingKeys.self)
    running = try c.decode(Bool.self, forKey: .running)
    warning = try c.decodeIfPresent(String.self, forKey: .warning)
    if let n = try? c.decodeIfPresent(Int.self, forKey: .found) {
      found = n
    } else {
      found = try c.decodeIfPresent(Legacy.self, forKey: .found)?.found
    }
  }
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
