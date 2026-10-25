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

      if model.isWorking || !model.steps.isEmpty {
        progress
      }

      presenceCard

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

extension ContentView {
  /// Dit toestel in Stulp, en thuis/weg via de geofence.
  fileprivate var presenceCard: some View {
    VStack(alignment: .leading, spacing: 12) {
      Text("Thuis en weg").font(.headline)
      Menu {
        ForEach(model.phones) { phone in
          Button(phone.name) { model.choosePhone(phone) }
        }
      } label: {
        Label(
          model.presence.map { "Dit toestel: \($0.phoneName)" } ?? "Kies welk toestel dit is",
          systemImage: "iphone")
      }
      .task { await model.loadPhones() }

      if model.presence != nil {
        Button {
          Task { await model.setHomeHere() }
        } label: {
          Label(
            model.presence?.latitude == nil ? "Stel thuis in op deze plek" : "Zet thuis opnieuw op deze plek",
            systemImage: "house.fill")
        }
        .disabled(model.isWorking)
        HStack {
          Button("Meld thuis") { Task { await model.report(home: true) } }
          Button("Meld weg") { Task { await model.report(home: false) } }
        }
        .font(.subheadline)
        Text(
          "Binnenkomen binnen 300 m meldt thuis, 150 m verlaten meldt weg; iOS doet dat ook als de app dicht is. Geef de app daarvoor locatie \"Altijd\". Aan de lader: maak in Opdrachten een automatisering op \"Lader is aangesloten\" en \"losgekoppeld\" met de actie \"Meld lader aan Stulp\"."
        )
        .font(.footnote)
        .foregroundStyle(.secondary)
      }
    }
    .padding(12)
    .background(Color(.tertiarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 12))
  }

  /// De mijlpalen van de lopende koppeling, met de tijd sinds de start, en
  /// de waarschuwing plus annuleerknop zodra Apple's fase blijft hangen.
  fileprivate var progress: some View {
    VStack(alignment: .leading, spacing: 8) {
      let start = model.steps.first?.at
      ForEach(Array(model.steps.suffix(8).enumerated()), id: \.offset) { _, step in
        HStack(alignment: .top, spacing: 8) {
          Text(start.map { String(format: "%3.0fs", step.at.timeIntervalSince($0)) } ?? "")
            .font(.caption.monospacedDigit())
            .foregroundStyle(.secondary)
          Text(step.text)
            .font(.caption)
        }
      }
      if model.isWorking && model.steps.isEmpty {
        Text("Wachten op iOS: QR-code, Bluetooth en netwerk…")
          .font(.caption)
          .foregroundStyle(.secondary)
      }
      if !model.hint.isEmpty {
        Label(model.hint, systemImage: "exclamationmark.triangle.fill")
          .font(.footnote)
          .foregroundStyle(.orange)
      }
      if model.isWorking {
        Button("Annuleer", role: .cancel) { model.cancelAdding() }
          .font(.subheadline)
      }
    }
    .padding(12)
    .background(Color(.tertiarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 12))
  }
}

extension View {
  fileprivate func card() -> some View {
    padding(20)
      .background(Color(.secondarySystemGroupedBackground), in: RoundedRectangle(cornerRadius: 20))
  }
}
