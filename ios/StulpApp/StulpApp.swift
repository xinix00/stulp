import SwiftUI

@main
struct StulpApp: App {
  @StateObject private var model = StulpAppModel()

  init() {
    // iOS wekt de app op de achtergrond voor een thuis/weg-overgang; de
    // geofence moet dan direct weer luisteren om het event te ontvangen.
    PresenceMonitor.shared.start()
  }

  var body: some Scene {
    WindowGroup {
      ContentView(model: model)
    }
  }
}
