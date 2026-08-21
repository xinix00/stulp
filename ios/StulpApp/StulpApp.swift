import SwiftUI

@main
struct StulpApp: App {
  @StateObject private var model = StulpAppModel()

  var body: some Scene {
    WindowGroup {
      ContentView(model: model)
    }
  }
}
