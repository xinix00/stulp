import SwiftUI
import WebKit

/// Manage, met alle apps erin, als ingebouwde browser. De eerste keer laadt hij
/// de Manage-link met de sleutel; Stulp zet dan een sessiecookie en die blijft
/// in de standaard websiteopslag van de app bewaard. Links naar andere sites
/// gaan naar Safari.
struct StulpWebView: UIViewRepresentable {
  let connection: StulpConnection
  @Binding var reloadToken: Int
  /// Een apparaat om te openen, bijvoorbeeld de camera van een melding.
  @Binding var openDevice: String?

  func makeCoordinator() -> Coordinator { Coordinator(host: connection.baseURL.host) }

  func makeUIView(context: Context) -> WKWebView {
    let configuration = WKWebViewConfiguration()
    configuration.allowsInlineMediaPlayback = true
    configuration.mediaTypesRequiringUserActionForPlayback = []
    configuration.websiteDataStore = .default()
    let view = WKWebView(frame: .zero, configuration: configuration)
    // Geen witte flits bij het laden en bij doortrekken: Manage's `--bg`.
    let background = UIColor(red: 0x0B / 255, green: 0x0B / 255, blue: 0x0C / 255, alpha: 1)
    view.isOpaque = false
    view.backgroundColor = background
    view.scrollView.backgroundColor = background
    view.underPageBackgroundColor = background
    view.navigationDelegate = context.coordinator
    view.uiDelegate = context.coordinator
    view.allowsBackForwardNavigationGestures = true
    view.scrollView.contentInsetAdjustmentBehavior = .automatic
    let refresh = UIRefreshControl()
    refresh.addTarget(
      context.coordinator, action: #selector(Coordinator.refresh(_:)), for: .valueChanged)
    view.scrollView.refreshControl = refresh
    context.coordinator.view = view
    view.load(URLRequest(url: connection.entryURL))
    return view
  }

  func updateUIView(_ view: WKWebView, context: Context) {
    if context.coordinator.reloadToken != reloadToken {
      context.coordinator.reloadToken = reloadToken
      view.load(URLRequest(url: connection.entryURL))
    }
    if let id = openDevice {
      // Manage opent `#device=<id>` zelf, ook als de pagina al open staat.
      let literal = (try? JSONSerialization.data(withJSONObject: [id]))
        .flatMap { String(data: $0, encoding: .utf8) } ?? "[]"
      view.evaluateJavaScript("location.hash = 'device=' + encodeURIComponent(\(literal)[0])")
      DispatchQueue.main.async { openDevice = nil }
    }
  }

  @MainActor
  final class Coordinator: NSObject, WKNavigationDelegate, WKUIDelegate {
    let host: String?
    weak var view: WKWebView?
    var reloadToken = 0

    init(host: String?) { self.host = host }

    @objc func refresh(_ control: UIRefreshControl) {
      view?.reload()
      control.endRefreshing()
    }

    func webView(
      _ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
      decisionHandler: @escaping @MainActor (WKNavigationActionPolicy) -> Void
    ) {
      // Alleen een aangeklikte link naar een ander domein gaat naar Safari;
      // iframes van apps en eigen navigatie blijven hier.
      if action.navigationType == .linkActivated, let url = action.request.url,
        url.host != host, ["http", "https"].contains(url.scheme ?? "")
      {
        UIApplication.shared.open(url)
        decisionHandler(.cancel)
        return
      }
      decisionHandler(.allow)
    }

    // target="_blank" in dezelfde weergave openen in plaats van te negeren.
    func webView(
      _ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
      for action: WKNavigationAction, windowFeatures: WKWindowFeatures
    ) -> WKWebView? {
      if let url = action.request.url {
        if url.host == host { webView.load(action.request) } else { UIApplication.shared.open(url) }
      }
      return nil
    }
  }
}
