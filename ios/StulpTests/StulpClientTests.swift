import Foundation
import Testing

#if SWIFT_PACKAGE
  @testable import StulpCompanionCore
#endif

@Suite("Stulp Matter API", .serialized)
struct StulpClientTests {
  @Test func commissioningUsesCookiePollsAddsEveryCandidateAndClosesPair() async throws {
    let observations = Locked(CommissionObservations())
    MockURLProtocol.handler = { request in
      observations.update { $0.paths.append(request.url!.path) }

      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/; HttpOnly"])
      case ("POST", "/api/stulp/pair"):
        observations.update { $0.cookie = request.value(forHTTPHeaderField: "Cookie") }
        return .json(
          #"{"id":"pair-1","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-1/emit/commission"):
        guard
          let json = try JSONSerialization.jsonObject(with: requestBody(request))
            as? [String: String]
        else {
          throw StubError.invalidRequest
        }
        observations.update { $0.onboardingCode = json["code"] }
        return .json(#"{"running":true}"#)
      case ("POST", "/api/stulp/pair/pair-1/emit/commission_state"):
        let poll = observations.update { value -> Int in
          value.statePolls += 1
          return value.statePolls
        }
        return poll == 1
          ? .json(#"{"running":true}"#)
          : .json(#"{"running":false,"found":{"found":2}}"#)
      case ("POST", "/api/stulp/pair/pair-1/emit/list_devices"):
        return .json(
          #"[{"name":"Lamp","data":{"nodeId":"1"}},{"name":"Sensor","data":{"nodeId":"2"}}]"#)
      case ("POST", "/api/stulp/apps/com.stulp.matter/drivers/matter/pair/devices"):
        guard
          let candidate = try JSONSerialization.jsonObject(with: requestBody(request))
            as? [String: Any],
          let name = candidate["name"] as? String
        else {
          throw StubError.invalidRequest
        }
        let index = observations.update { value -> Int in
          value.addedNames.append(name)
          return value.addedNames.count
        }
        return .json("{\"id\":\"device-\(index)\",\"name\":\"\(name)\"}", status: 201)
      case ("DELETE", "/api/stulp/pair/pair-1"):
        return .json("true")
      default:
        observations.update {
          $0.unexpectedRequest = "\(request.httpMethod ?? "") \(request.url!.path)"
        }
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    let client = StulpClient(
      connection: StulpConnection(baseURL: URL(string: "https://stulp.test")!, accessKey: "secret"),
      configuration: mockConfiguration(),
      pollIntervalNanoseconds: 1
    )
    let devices = try await client.commissionMatter(onboardingPayload: "MT:TEST")
    let found = observations.read()

    #expect(
      devices == [
        PendingMatterDevice(id: "device-1", name: "Lamp"),
        PendingMatterDevice(id: "device-2", name: "Sensor"),
      ])
    #expect(found.cookie == "stulp-session=proof")
    #expect(found.onboardingCode == "MT:TEST")
    #expect(found.addedNames == ["Lamp", "Sensor"])
    #expect(found.paths.last == "/api/stulp/pair/pair-1")
    #expect(found.unexpectedRequest == nil)
  }

  @Test func commissioningWarningStillClosesPair() async {
    let didClose = Locked(false)
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("POST", "/api/stulp/pair"):
        return .json(
          #"{"id":"pair-2","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-2/emit/commission"):
        return .json(#"{"running":false,"warning":"apparaat niet gevonden"}"#)
      case ("DELETE", "/api/stulp/pair/pair-2"):
        didClose.update { $0 = true }
        return .json(#"{"error":"cleanup failed"}"#, status: 500)
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    let client = StulpClient(
      connection: StulpConnection(baseURL: URL(string: "https://stulp.test")!, accessKey: "secret"),
      configuration: mockConfiguration(),
      pollIntervalNanoseconds: 1
    )
    do {
      _ = try await client.commissionMatter(onboardingPayload: "MT:TEST")
      Issue.record("Expected commissioning failure")
    } catch {
      #expect(error as? StulpClientError == .commissioningFailed("apparaat niet gevonden"))
    }
    #expect(didClose.read())
  }

  @Test func successfulCommissioningIsNotTurnedIntoFailureByCleanup() async throws {
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("POST", "/api/stulp/pair"):
        return .json(
          #"{"id":"pair-3","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-3/emit/commission"):
        return .json(#"{"running":false,"found":{"found":1}}"#)
      case ("POST", "/api/stulp/pair/pair-3/emit/list_devices"):
        return .json(#"[{"name":"Lamp","data":{"nodeId":"1"}}]"#)
      case ("POST", "/api/stulp/apps/com.stulp.matter/drivers/matter/pair/devices"):
        return .json(#"{"id":"device-1","name":"Lamp"}"#, status: 201)
      case ("DELETE", "/api/stulp/pair/pair-3"):
        return .json(#"{"error":"cleanup failed"}"#, status: 500)
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    let devices = try await makeClient().commissionMatter(onboardingPayload: "MT:TEST")
    #expect(devices == [PendingMatterDevice(id: "device-1", name: "Lamp")])
  }

  @Test func candidateCommitRetriesAfterTransientServerFailure() async throws {
    let attempts = Locked(0)
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("POST", "/api/stulp/pair"):
        return .json(
          #"{"id":"pair-retry","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-retry/emit/commission"):
        return .json(#"{"running":false,"found":{"found":1}}"#)
      case ("POST", "/api/stulp/pair/pair-retry/emit/list_devices"):
        return .json(#"[{"name":"Lamp","data":{"nodeId":"1"}}]"#)
      case ("POST", "/api/stulp/apps/com.stulp.matter/drivers/matter/pair/devices"):
        let attempt = attempts.update { value -> Int in
          value += 1
          return value
        }
        return attempt == 1
          ? .json(#"{"error":"try again"}"#, status: 503)
          : .json(#"{"id":"device-1","name":"Lamp"}"#, status: 201)
      case ("DELETE", "/api/stulp/pair/pair-retry"):
        return .json("true")
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    let devices = try await makeClient().commissionMatter(onboardingPayload: "MT:TEST")
    #expect(devices == [PendingMatterDevice(id: "device-1", name: "Lamp")])
    #expect(attempts.read() == 2)
  }

  @Test func pollDeadlineClosesPair() async {
    let didClose = Locked(false)
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("POST", "/api/stulp/pair"):
        return .json(
          #"{"id":"pair-4","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-4/emit/commission"):
        return .json(#"{"running":true}"#)
      case ("DELETE", "/api/stulp/pair/pair-4"):
        didClose.update { $0 = true }
        return .json("true")
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    do {
      _ = try await makeClient(commissioningTimeout: .zero)
        .commissionMatter(onboardingPayload: "MT:TEST")
      Issue.record("Expected polling timeout")
    } catch {
      #expect(error as? StulpClientError == .commissioningTimedOut)
    }
    #expect(didClose.read())
  }

  @Test func cancellationUsesIndependentCleanupTask() async {
    let started = Locked(false)
    let closes = Locked(0)
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("POST", "/api/stulp/pair"):
        return .json(
          #"{"id":"pair-5","handlers":["commission","commission_state","list_devices"]}"#,
          status: 201)
      case ("POST", "/api/stulp/pair/pair-5/emit/commission"):
        started.update { $0 = true }
        return .json(#"{"running":true}"#)
      case ("DELETE", "/api/stulp/pair/pair-5"):
        closes.update { $0 += 1 }
        return .json("true")
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    let client = makeClient(pollIntervalNanoseconds: 1_000_000_000)
    let work = Task { try await client.commissionMatter(onboardingPayload: "MT:TEST") }
    while !started.read() { await Task.yield() }
    work.cancel()
    do {
      _ = try await work.value
      Issue.record("Expected cancellation")
    } catch {
      let urlCancellation = (error as? URLError)?.code == .cancelled
      #expect(error is CancellationError || urlCancellation)
    }
    #expect(closes.read() >= 1)
  }

  @Test func apiNotFoundIsNotReportedAsBadManageKey() async {
    MockURLProtocol.handler = { request in
      switch (request.httpMethod, request.url!.path) {
      case ("GET", "/secret"):
        return .json("{}", headers: ["Set-Cookie": "stulp-session=proof; Path=/"])
      case ("GET", "/api/stulp/health"):
        return .json(#"{"error":"route missing"}"#, status: 404)
      default:
        return .json(#"{"error":"unexpected"}"#, status: 500)
      }
    }
    defer { MockURLProtocol.handler = nil }

    do {
      _ = try await makeClient().validate()
      Issue.record("Expected missing API route")
    } catch {
      #expect(error as? StulpClientError == .server(status: 404, message: "route missing"))
    }
  }

  private func makeClient(
    pollIntervalNanoseconds: UInt64 = 1,
    commissioningTimeout: Duration = .seconds(195)
  ) -> StulpClient {
    StulpClient(
      connection: StulpConnection(
        baseURL: URL(string: "https://stulp.test")!, accessKey: "secret"),
      configuration: mockConfiguration(),
      pollIntervalNanoseconds: pollIntervalNanoseconds,
      commissioningTimeout: commissioningTimeout,
      candidateRetryDelayNanoseconds: 1
    )
  }

  private func mockConfiguration() -> URLSessionConfiguration {
    let configuration = URLSessionConfiguration.ephemeral
    configuration.protocolClasses = [MockURLProtocol.self]
    return configuration
  }
}

private struct CommissionObservations {
  var paths: [String] = []
  var statePolls = 0
  var addedNames: [String] = []
  var cookie: String?
  var onboardingCode: String?
  var unexpectedRequest: String?
}

private final class Locked<Value>: @unchecked Sendable {
  private let lock = NSLock()
  private var value: Value

  init(_ value: Value) { self.value = value }

  func read() -> Value { lock.withLock { value } }

  @discardableResult
  func update<Result>(_ body: (inout Value) throws -> Result) rethrows -> Result {
    try lock.withLock { try body(&value) }
  }
}

private final class MockURLProtocol: URLProtocol, @unchecked Sendable {
  nonisolated(unsafe) static var handler: (@Sendable (URLRequest) throws -> StubResponse)?

  override class func canInit(with request: URLRequest) -> Bool { true }
  override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

  override func startLoading() {
    do {
      guard let handler = Self.handler else { throw StubError.noHandler }
      let answer = try handler(request)
      let response = HTTPURLResponse(
        url: request.url!,
        statusCode: answer.status,
        httpVersion: "HTTP/1.1",
        headerFields: answer.headers
      )!
      client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
      client?.urlProtocol(self, didLoad: answer.data)
      client?.urlProtocolDidFinishLoading(self)
    } catch {
      client?.urlProtocol(self, didFailWithError: error)
    }
  }

  override func stopLoading() {}
}

private enum StubError: Error {
  case noHandler
  case invalidRequest
}

private func requestBody(_ request: URLRequest) throws -> Data {
  if let body = request.httpBody { return body }
  guard let stream = request.httpBodyStream else { throw StubError.invalidRequest }
  stream.open()
  defer { stream.close() }
  var result = Data()
  var buffer = [UInt8](repeating: 0, count: 1024)
  while stream.hasBytesAvailable {
    let count = stream.read(&buffer, maxLength: buffer.count)
    if count < 0 { throw stream.streamError ?? StubError.invalidRequest }
    if count == 0 { break }
    result.append(buffer, count: count)
  }
  return result
}

private struct StubResponse: Sendable {
  let status: Int
  let headers: [String: String]
  let data: Data

  static func json(_ body: String, status: Int = 200, headers: [String: String] = [:])
    -> StubResponse
  {
    var allHeaders = headers
    allHeaders["Content-Type"] = "application/json"
    return StubResponse(status: status, headers: allHeaders, data: Data(body.utf8))
  }
}
