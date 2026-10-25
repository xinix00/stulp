import SwiftUI
import UIKit
import UserNotifications
import os

/// Meldingen via de app: toestemming, het APNs-token naar Stulp, en wat er
/// gebeurt als je op een melding tikt (een camera opent in Thuis).
@MainActor
final class PushCenter: NSObject, ObservableObject, UNUserNotificationCenterDelegate {
  static let shared = PushCenter()

  @Published private(set) var status: UNAuthorizationStatus = .notDetermined
  @Published private(set) var registered = false
  @Published var lastError = ""
  /// Gezet na een tik op een melding met een camera; Thuis opent hem.
  @Published var openDevice: String?

  private var token: String?
  private let logger = Logger(subsystem: "software.haas.stulp", category: "Push")

  /// Bij elke start: iOS kan het token wisselen, dus altijd opnieuw registreren
  /// als er al toestemming is.
  func start() {
    UNUserNotificationCenter.current().delegate = self
    Task { await refresh(register: true) }
  }

  func request() async {
    do {
      // Kritiek (.criticalAlert) pas als Apple de app daarvoor heeft
      // goedgekeurd; tot dan komt kritiek binnen als dringend.
      let granted = try await UNUserNotificationCenter.current()
        .requestAuthorization(options: [.alert, .sound, .badge])
      if !granted { lastError = "Meldingen staan uit voor Stulp; zet ze aan in Instellingen." }
    } catch {
      lastError = error.localizedDescription
    }
    await refresh(register: true)
  }

  private func refresh(register: Bool) async {
    let settings = await UNUserNotificationCenter.current().notificationSettings()
    status = settings.authorizationStatus
    if register, [.authorized, .provisional, .ephemeral].contains(settings.authorizationStatus) {
      UIApplication.shared.registerForRemoteNotifications()
    }
  }

  func didRegister(_ deviceToken: Data) async {
    let token = deviceToken.map { String(format: "%02x", $0) }.joined()
    self.token = token
    await send()
  }

  func didFail(_ error: any Error) {
    lastError = "Apple gaf geen push-token: \(error.localizedDescription)"
  }

  /// Ook na het kiezen of registreren van een toestel: het token hoort bij dat apparaat.
  func send() async {
    guard let token else { return }
    guard let connection = try? SharedStulpConfiguration.load(),
      let settings = StulpPresenceSettings.load(), !settings.phoneId.isEmpty
    else { return }
    do {
      try await StulpClient(connection: connection, logSource: "ios-push")
        .registerPush(deviceId: settings.phoneId, token: token, environment: Self.environment)
      registered = true
      lastError = ""
    } catch {
      registered = false
      lastError = "Push-token niet bij Stulp: \(error.localizedDescription)"
      logger.error("register_push: \(error.localizedDescription, privacy: .public)")
    }
  }

  /// Een build uit Xcode praat met de APNs-sandbox, TestFlight en de App Store
  /// met productie. Alleen een ontwikkelprofiel zit nog in de app; dat zegt het.
  static var environment: String {
    guard let url = Bundle.main.url(forResource: "embedded", withExtension: "mobileprovision"),
      let data = try? Data(contentsOf: url),
      let text = String(data: data, encoding: .isoLatin1),
      let key = text.range(of: "<key>aps-environment</key>"),
      let open = text.range(of: "<string>", range: key.upperBound..<text.endIndex),
      let close = text.range(of: "</string>", range: open.upperBound..<text.endIndex)
    else { return "production" }
    return text[open.upperBound..<close.lowerBound] == "development" ? "development" : "production"
  }

  nonisolated func userNotificationCenter(
    _ center: UNUserNotificationCenter, willPresent notification: UNNotification
  ) async -> UNNotificationPresentationOptions {
    [.banner, .sound, .list]
  }

  nonisolated func userNotificationCenter(
    _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse
  ) async {
    let stulp = response.notification.request.content.userInfo["stulp"] as? [String: Any]
    let camera = stulp?["camera"] as? String
    guard let camera, !camera.isEmpty else { return }
    await MainActor.run { self.openDevice = camera }
  }
}

/// UIKit's callbacks voor het APNs-token; SwiftUI heeft er geen eigen weg voor.
final class StulpAppDelegate: NSObject, UIApplicationDelegate {
  func application(
    _ application: UIApplication,
    didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data
  ) {
    Task { await PushCenter.shared.didRegister(deviceToken) }
  }

  func application(
    _ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: any Error
  ) {
    Task { @MainActor in PushCenter.shared.didFail(error) }
  }
}
