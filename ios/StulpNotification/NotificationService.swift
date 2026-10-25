import UserNotifications

/// Haalt het camerabeeld bij een melding op. Stulp stuurt alleen het adres
/// (`stulp.image`), want 4 KiB payload past geen foto; dat adres is een kwartier
/// geldig, niet te raden, en heeft geen sleutel nodig.
///
/// iOS roept `didReceive` en `serviceExtensionTimeWillExpire` op zijn eigen
/// queue aan en de download eindigt op een andere; een slot zorgt dat de
/// melding precies één keer doorgaat.
final class NotificationService: UNNotificationServiceExtension, @unchecked Sendable {
  private let lock = NSLock()
  private var handler: ((UNNotificationContent) -> Void)?
  private var content: UNMutableNotificationContent?

  override func didReceive(
    _ request: UNNotificationRequest,
    withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void
  ) {
    let content = (request.content.mutableCopy() as? UNMutableNotificationContent) ?? UNMutableNotificationContent()
    lock.withLock {
      handler = contentHandler
      self.content = content
    }
    guard
      let stulp = request.content.userInfo["stulp"] as? [String: Any],
      let address = stulp["image"] as? String,
      let url = URL(string: address), url.scheme == "https" || url.scheme == "http"
    else {
      contentHandler(content)
      return
    }
    let task = URLSession.shared.downloadTask(with: url) { file, response, _ in
      defer { self.finish() }
      guard let file, let content = self.lock.withLock({ self.content }) else { return }
      let type = (response as? HTTPURLResponse)?.value(forHTTPHeaderField: "Content-Type") ?? ""
      let ext = type.contains("png") ? "png" : "jpg"
      let target = FileManager.default.temporaryDirectory
        .appendingPathComponent(UUID().uuidString).appendingPathExtension(ext)
      guard (try? FileManager.default.moveItem(at: file, to: target)) != nil,
        let attachment = try? UNNotificationAttachment(identifier: "camera", url: target)
      else { return }
      content.attachments = [attachment]
    }
    task.resume()
  }

  private func finish() {
    let pending = lock.withLock { () -> (((UNNotificationContent) -> Void), UNNotificationContent)? in
      guard let handler, let content else { return nil }
      self.handler = nil
      return (handler, content)
    }
    if let (handler, content) = pending { handler(content) }
  }

  /// iOS geeft de extensie ongeveer 30 seconden; zonder beeld gaat de melding gewoon door.
  override func serviceExtensionTimeWillExpire() {
    finish()
  }
}
