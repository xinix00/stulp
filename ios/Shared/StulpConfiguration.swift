import Foundation
import Security

struct StulpConnection: Codable, Equatable, Sendable {
  let baseURL: URL
  let accessKey: String

  var entryURL: URL {
    baseURL.appending(path: accessKey)
  }

  static func parse(_ input: String) throws -> StulpConnection {
    var value = input.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !value.isEmpty else { throw StulpConnectionError.empty }
    if !value.contains("://") { value = "https://" + value }

    guard var components = URLComponents(string: value),
      let scheme = components.scheme?.lowercased(),
      scheme == "https" || scheme == "http",
      components.host != nil,
      components.user == nil,
      components.password == nil,
      components.query == nil,
      components.fragment == nil
    else {
      throw StulpConnectionError.invalidURL
    }

    let key =
      components.percentEncodedPath
      .trimmingCharacters(in: CharacterSet(charactersIn: "/"))
      .removingPercentEncoding?
      .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
    guard !key.isEmpty, !key.contains("/") else {
      throw StulpConnectionError.missingKey
    }

    components.path = ""
    components.query = nil
    components.fragment = nil
    guard let baseURL = components.url else { throw StulpConnectionError.invalidURL }
    return StulpConnection(baseURL: baseURL, accessKey: key)
  }
}

enum StulpConnectionError: LocalizedError, Equatable {
  case empty
  case invalidURL
  case missingKey

  var errorDescription: String? {
    switch self {
    case .empty:
      "Plak eerst de volledige Manage-link."
    case .invalidURL:
      "Dit is geen geldige http- of https-link naar Stulp."
    case .missingKey:
      "De link mist de toegangssleutel achter de hostnaam."
    }
  }
}

enum SharedStulpConfiguration {
  private static let service = "software.haas.stulp.connection"
  private static let account = "primary"

  static func load() throws -> StulpConnection? {
    var query = try baseQuery()
    query[kSecReturnData as String] = true
    query[kSecMatchLimit as String] = kSecMatchLimitOne
    var item: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &item)
    if status == errSecItemNotFound { return nil }
    guard status == errSecSuccess, let data = item as? Data else {
      throw KeychainError(status: status)
    }
    return try JSONDecoder().decode(StulpConnection.self, from: data)
  }

  static func save(_ connection: StulpConnection) throws {
    let data = try JSONEncoder().encode(connection)
    let values: [String: Any] = [
      kSecValueData as String: data,
      kSecAttrAccessible as String: kSecAttrAccessibleWhenUnlockedThisDeviceOnly,
    ]
    let query = try baseQuery()
    let status = SecItemUpdate(query as CFDictionary, values as CFDictionary)
    if status == errSecSuccess { return }
    guard status == errSecItemNotFound else { throw KeychainError(status: status) }

    var item = query
    for (key, value) in values {
      item[key] = value
    }
    let addStatus = SecItemAdd(item as CFDictionary, nil)
    guard addStatus == errSecSuccess else { throw KeychainError(status: addStatus) }
  }

  static func remove() throws {
    let status = SecItemDelete(try baseQuery() as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else {
      throw KeychainError(status: status)
    }
  }

  private static func baseQuery() throws -> [String: Any] {
    guard
      let group = Bundle.main.object(forInfoDictionaryKey: "StulpKeychainAccessGroup") as? String,
      !group.isEmpty,
      !group.contains("$(")
    else {
      throw SharedConfigurationError.missingKeychainAccessGroup
    }
    return [
      kSecClass as String: kSecClassGenericPassword,
      kSecAttrService as String: service,
      kSecAttrAccount as String: account,
      kSecAttrAccessGroup as String: group,
    ]
  }
}

private struct KeychainError: LocalizedError {
  let status: OSStatus

  var errorDescription: String? {
    if let message = SecCopyErrorMessageString(status, nil) as String? {
      return "De Stulp-verbinding kon niet veilig worden bewaard: \(message)"
    }
    return "De Stulp-verbinding kon niet veilig worden bewaard (\(status))."
  }
}

struct PendingMatterDevice: Codable, Equatable, Sendable {
  let id: String
  let name: String
}

enum PendingMatterDevices {
  private static let key = "pending-matter-devices"

  static func load() throws -> [PendingMatterDevice] {
    guard let data = try defaults().data(forKey: key) else { return [] }
    return (try? JSONDecoder().decode([PendingMatterDevice].self, from: data)) ?? []
  }

  static func save(_ devices: [PendingMatterDevice]) throws {
    try defaults().set(try JSONEncoder().encode(devices), forKey: key)
  }

  static func remove() throws {
    try defaults().removeObject(forKey: key)
  }

  private static func defaults() throws -> UserDefaults {
    try sharedDefaults()
  }
}

func sharedDefaults() throws -> UserDefaults {
  guard
    let identifier = Bundle.main.object(forInfoDictionaryKey: "StulpAppGroup") as? String,
    !identifier.isEmpty,
    !identifier.contains("$("),
    let shared = UserDefaults(suiteName: identifier)
  else {
    throw SharedConfigurationError.missingAppGroup
  }
  return shared
}

/// Eén mijlpaal van een koppeling, door de extensie gezet en door de app
/// live getoond. Apple's eigen fase is voor de app anders onzichtbaar, en
/// juist daar blijft het soms hangen: na de netwerkstap hoort de overdracht
/// aan Stulp binnen seconden te volgen.
struct MatterProgressStep: Codable, Equatable, Sendable {
  let at: Date
  let text: String
}

enum MatterProgress {
  private static let key = "matter-progress"
  static let maximum = 40

  static func clear() {
    try? sharedDefaults().removeObject(forKey: key)
  }

  static func record(_ text: String) {
    var steps = load()
    steps.append(MatterProgressStep(at: Date(), text: text))
    if steps.count > maximum { steps.removeFirst(steps.count - maximum) }
    if let data = try? JSONEncoder().encode(steps) {
      try? sharedDefaults().set(data, forKey: key)
    }
  }

  static func load() -> [MatterProgressStep] {
    guard let data = try? sharedDefaults().data(forKey: key) else { return [] }
    return (try? JSONDecoder().decode([MatterProgressStep].self, from: data)) ?? []
  }
}

enum SharedConfigurationError: LocalizedError {
  case missingKeychainAccessGroup
  case missingAppGroup

  var errorDescription: String? {
    switch self {
    case .missingKeychainAccessGroup:
      "De gedeelde Keychain-groep ontbreekt. Controleer signing voor de app en Matter-extensie."
    case .missingAppGroup:
      "De gedeelde App Group ontbreekt. Controleer signing voor de app en Matter-extensie."
    }
  }
}

/// Welk telefoon-apparaat in Stulp deze iPhone is, en waar thuis ligt. Gedeeld
/// met de extensies en de Opdrachten-acties via de app-group.
struct StulpPresenceSettings: Codable, Equatable, Sendable {
  var phoneId: String
  var phoneName: String
  var latitude: Double?
  var longitude: Double?
  /// Binnenkomen binnen deze straal: thuis. Ruim, zodat het alarm eraf kan
  /// vóór je bij de deur bent en hij niet loeit (25-10).
  var arriveRadius: Double = 300
  /// Deze straal verlaten: weg.
  var leaveRadius: Double = 150

  private static let key = "presence-settings"

  static func load() -> StulpPresenceSettings? {
    guard let data = try? sharedDefaults().data(forKey: key) else { return nil }
    return try? JSONDecoder().decode(StulpPresenceSettings.self, from: data)
  }

  func save() throws {
    try sharedDefaults().set(JSONEncoder().encode(self), forKey: Self.key)
  }

  static func remove() {
    try? sharedDefaults().removeObject(forKey: key)
  }
}

/// Eén melding van de telefoon naar Stulp (lader, thuis), vanuit de app of een
/// Opdrachten-actie. Zonder gekozen toestel of verbinding gebeurt er niets.
enum StulpPresenceReport {
  static func send(_ values: [String: Bool], source: String) async throws {
    guard let connection = try SharedStulpConfiguration.load() else {
      throw StulpPresenceError.notConnected
    }
    guard let settings = StulpPresenceSettings.load(), !settings.phoneId.isEmpty else {
      throw StulpPresenceError.noPhone
    }
    let client = StulpClient(connection: connection, logSource: source)
    try await client.report(deviceId: settings.phoneId, values: values)
    let text = values.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: " ")
    client.trace("gemeld: \(text)")
  }
}

enum StulpPresenceError: LocalizedError {
  case notConnected
  case noPhone

  var errorDescription: String? {
    switch self {
    case .notConnected: "Verbind de Stulp-app eerst met je Manage-link."
    case .noPhone: "Kies in de Stulp-app eerst welk toestel dit is."
    }
  }
}
