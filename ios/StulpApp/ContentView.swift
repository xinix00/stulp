import SwiftUI

struct ContentView: View {
  @ObservedObject var model: StulpAppModel

  var body: some View {
    NavigationStack {
      ScrollView {
        VStack(alignment: .leading, spacing: 24) {
          brand
          if let connection = model.connection {
            connected(connection)
          } else {
            setup
          }
          if !model.status.isEmpty {
            Text(model.status)
              .font(.footnote)
              .foregroundStyle(.secondary)
              .frame(maxWidth: .infinity, alignment: .leading)
              .accessibilityIdentifier("status")
          }
        }
        .padding(24)
      }
      .background(Color(.systemGroupedBackground))
      .navigationTitle("Stulp")
    }
  }

  private var brand: some View {
    VStack(alignment: .leading, spacing: 8) {
      Image(systemName: "house.and.flag.fill")
        .font(.system(size: 44))
        .foregroundStyle(Color.accentColor)
      Text("Je devices, direct naar huis.")
        .font(.title2.bold())
      Text(
        "iOS regelt de Matter-code, Bluetooth en netwerktoegang. Stulp blijft zelf de controller en bewaart zijn eigen fabric."
      )
      .foregroundStyle(.secondary)
    }
  }

  private var setup: some View {
    VStack(alignment: .leading, spacing: 16) {
      Text("Verbind één keer")
        .font(.headline)
      Text(
        "Plak de volledige Manage-link, inclusief de toegangssleutel. De app bewaart hem alleen in de gedeelde iOS-sleutelhanger."
      )
      .font(.subheadline)
      .foregroundStyle(.secondary)
      TextField("https://stulp.local/toegangssleutel", text: $model.entry)
        .textContentType(.URL)
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
        .keyboardType(.URL)
        .submitLabel(.go)
        .onSubmit { Task { await model.connect() } }
        .padding(12)
        .background(
          Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 12)
        )
        .accessibilityIdentifier("manage-url")
      Button {
        Task { await model.connect() }
      } label: {
        Label(model.isWorking ? "Controleren…" : "Verbind met Stulp", systemImage: "link")
          .frame(maxWidth: .infinity)
      }
      .buttonStyle(.borderedProminent)
      .disabled(
        model.isWorking || model.entry.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
    }
    .card()
  }

  private func connected(_ connection: StulpConnection) -> some View {
    VStack(alignment: .leading, spacing: 18) {
      Label(
        "Verbonden met \(connection.baseURL.host ?? "Stulp")", systemImage: "checkmark.circle.fill"
      )
      .font(.headline)
      .foregroundStyle(.green)

      Button {
        Task { await model.addMatterDevice() }
      } label: {
        VStack(spacing: 8) {
          Image(systemName: "plus.circle.fill")
            .font(.system(size: 34))
          Text(model.isWorking ? "Bezig…" : "Matter-apparaat toevoegen")
            .font(.headline)
          Text("Scan de QR-code op het nieuwe apparaat")
            .font(.caption)
            .opacity(0.8)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 12)
      }
      .buttonStyle(.borderedProminent)
      .disabled(model.isWorking)
      .accessibilityIdentifier("add-matter-device")

      Text(
        "Thread werkt wanneer iOS het default Thread-netwerk kent en Stulp dat via een blijvende border router kan bereiken, meestal een Apple TV of HomePod."
      )
      .font(.footnote)
      .foregroundStyle(.secondary)

      HStack {
        Link(destination: connection.entryURL) {
          Label("Open Manage", systemImage: "safari")
        }
        Spacer()
        Button("Wijzig verbinding", role: .destructive) {
          model.disconnect()
        }
      }
      .font(.subheadline)
    }
    .card()
  }
}

extension View {
  fileprivate func card() -> some View {
    padding(20)
      .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 20))
  }
}
