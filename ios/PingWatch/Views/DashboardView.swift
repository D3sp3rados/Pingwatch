import SwiftUI
import WebKit

/// Das komplette Web-Dashboard (RMM, Gruppen, UniFi/Protect, Fritzbox …) direkt in der App.
/// Angemeldet wird mit dem Token der App – keine zweite Anmeldung nötig. Neue Funktionen des
/// Dashboards erscheinen hier ohne App-Update.
struct DashboardView: View {
    @Environment(AppModel.self) private var model
    @State private var reload = 0

    var body: some View {
        Group {
            if let creds = model.creds {
                DashboardWebView(creds: creds, reload: reload)
            } else {
                ContentUnavailableView("Nicht angemeldet", systemImage: "person.crop.circle.badge.exclamationmark")
            }
        }
        .navigationTitle("Dashboard")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    reload += 1
                } label: {
                    Image(systemName: "arrow.clockwise")
                }
            }
        }
    }
}

struct DashboardWebView: UIViewRepresentable {
    let creds: Credentials
    let reload: Int

    final class Box {
        var lastReload = 0
    }

    func makeCoordinator() -> Box { Box() }

    func makeUIView(context: Context) -> WKWebView {
        let web = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        web.allowsBackForwardNavigationGestures = true
        let refresh = UIRefreshControl()
        refresh.addAction(UIAction { [weak web, weak refresh] _ in
            web?.reload()
            refresh?.endRefreshing()
        }, for: .valueChanged)
        web.scrollView.refreshControl = refresh
        context.coordinator.lastReload = reload
        load(web)
        return web
    }

    func updateUIView(_ web: WKWebView, context: Context) {
        if context.coordinator.lastReload != reload {
            context.coordinator.lastReload = reload
            load(web)
        }
    }

    private func load(_ web: WKWebView) {
        var req = URLRequest(url: creds.server.appendingPathComponent("api/app/websession"))
        req.httpMethod = "POST"
        req.setValue("Bearer \(creds.token)", forHTTPHeaderField: "Authorization")
        web.load(req)
    }
}
