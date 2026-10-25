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
        // Manage is altijd donker; de app volgt dat, zodat web en native
        // één geheel zijn in plaats van een witte balk boven een zwarte pagina.
        .preferredColorScheme(.dark)
        .tint(Color.accentColor)
    }
  }
}
