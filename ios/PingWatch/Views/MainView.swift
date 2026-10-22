import SwiftUI
import WebKit

struct MainView: View {
    @Environment(AppModel.self) private var model
    @State private var tab = 0
    @State private var path = NavigationPath()

    var body: some View {
        TabView(selection: $tab) {
            NavigationStack(path: $path) {
                OverviewView()
                    .navigationDestination(for: String.self) { HostDetailView(hostID: $0) }
            }
            .tabItem { Label("Übersicht", systemImage: "square.grid.2x2") }
            .tag(0)

            NavigationStack {
                EventsView(hostID: nil)
                    .navigationDestination(for: String.self) { HostDetailView(hostID: $0) }
            }
            .tabItem { Label("Ereignisse", systemImage: "list.bullet.rectangle") }
            .tag(1)

            NavigationStack {
                DashboardView()
            }
            .tabItem { Label("Dashboard", systemImage: "rectangle.3.group") }
            .tag(3)

            NavigationStack {
                SettingsView()
            }
            .tabItem { Label("Einstellungen", systemImage: "gearshape") }
            .tag(2)
        }
        .task {
            // regelmäßig aktualisieren, solange die App offen ist
            while !Task.isCancelled {
                await model.refresh()
                try? await Task.sleep(for: .seconds(5))
            }
        }
        .onChange(of: model.openHostID) { _, id in
            guard let id else { return }
            tab = 0
            path = NavigationPath()
            path.append(id)
            model.openHostID = nil
        }
    }
}

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
