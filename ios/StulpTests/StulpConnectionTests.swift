import Foundation
import Testing

#if SWIFT_PACKAGE
  @testable import StulpCompanionCore
#endif

@Suite("Stulp Manage-link")
struct StulpConnectionTests {
  @Test func parsesFullManageURLIntoOriginAndKey() throws {
    let connection = try StulpConnection.parse("https://stulp.example:8443/geheime-sleutel")
    #expect(connection.baseURL.absoluteString == "https://stulp.example:8443")
    #expect(connection.accessKey == "geheime-sleutel")
    #expect(connection.entryURL.absoluteString == "https://stulp.example:8443/geheime-sleutel")
  }

  @Test func addsHTTPSWhenSchemeIsOmitted() throws {
    let connection = try StulpConnection.parse("stulp.local/sleutel")
    #expect(connection.baseURL.absoluteString == "https://stulp.local")
    #expect(connection.accessKey == "sleutel")
  }

  @Test func rejectsURLWithoutAccessKey() {
    #expect(throws: StulpConnectionError.missingKey) {
      try StulpConnection.parse("https://stulp.local")
    }
  }

  @Test func rejectsCredentialsAndQueries() {
    #expect(throws: StulpConnectionError.invalidURL) {
      try StulpConnection.parse("https://user:pass@stulp.local/key")
    }
    #expect(throws: StulpConnectionError.invalidURL) {
      try StulpConnection.parse("https://stulp.local/key?leak=yes")
    }
  }
}
