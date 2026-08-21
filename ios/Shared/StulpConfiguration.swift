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
  private static let service = "com.xinix00.stulp.connection"
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
