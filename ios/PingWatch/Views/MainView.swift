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
                NetMapListView()
                    .navigationDestination(for: String.self) { HostDetailView(hostID: $0) }
            }
            .tabItem { Label("Netz", systemImage: "point.3.connected.trianglepath.dotted") }
            .tag(4)

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


// MARK: - Netzkarte als Liste (Orte → Sektoren → Kunden)

@MainActor @Observable
final class NetMapStore {
    var tree: MapTree?
    var error: String?
    var loading = false
    var mapID: String?
    var loadedAt = Date.distantPast

    func load(_ model: AppModel, force: Bool = false) async {
        guard let api = model.api else { return }
        if !force && Date().timeIntervalSince(loadedAt) < 30 && tree != nil { return }
        loading = true
        defer { loading = false }
        do {
            tree = try await api.mapTree(id: mapID, hideOld: UserDefaults.standard.bool(forKey: "mapHideOld"))
            mapID = tree?.map?.id
            error = nil
            loadedAt = Date()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

private func mapColor(_ st: String) -> Color {
    switch st {
    case "up": return PW.ok
    case "down": return PW.bad
    case "warn": return PW.warn
    case "paused": return .yellow
    default: return .gray
    }
}

private func mapStatusText(_ n: MapTreeNode) -> String {
    switch n.st {
    case "up": return n.rttText.map { "erreichbar · \($0)" } ?? "erreichbar"
    case "down": return "nicht erreichbar"
    case "warn": return "Paketverlust"
    case "paused": return "pausiert"
    default: return "unbekannt"
    }
}

struct NetMapListView: View {
    @Environment(AppModel.self) private var model
    @State private var store = NetMapStore()
    @State private var query = ""
    @State private var onlyDown = false
    @AppStorage("mapHideOld") private var hideOld = false

    var body: some View {
        List {
            if let e = store.error {
                Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(PW.bad)
            }
            if let t = store.tree {
                if (t.maps?.count ?? 0) > 1 {
                    Picker("Karte", selection: Binding(get: { store.mapID ?? "" }, set: { store.mapID = $0; Task { await store.load(model, force: true) } })) {
                        ForEach(t.maps ?? []) { Text($0.name).tag($0.id) }
                    }
                }
                if t.checked != true {
                    Text("Erreichbarkeit wird noch nicht geprüft – im Web-Dashboard unter Netzkarte „Erreichbarkeit über …“ einschalten.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                if !query.isEmpty {
                    let f = query.lowercased()
                    let hits = t.zones.flatMap { z in z.sectors.flatMap { [$0] + ($0.kids ?? []) } }
                        .filter { $0.name.lowercased().contains(f) || ($0.addr ?? "").contains(f) }
                    Section("Treffer (\(hits.count))") {
                        ForEach(hits.prefix(200)) { n in NavigationLink { MapNodeView(node: n) } label: { MapNodeRow(node: n) } }
                    }
                } else {
                    let zs = t.zones.filter { !onlyDown || $0.down > 0 }
                    Section {
                        ForEach(zs) { z in
                            NavigationLink { MapZoneView(zone: z, onlyDown: onlyDown) } label: {
                                HStack(spacing: 10) {
                                    Circle().fill(z.down > 0 ? PW.bad : PW.ok).frame(width: 10, height: 10)
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(z.name).font(.headline)
                                        Text("\(z.sectors.filter { ($0.kids ?? []).count > 0 }.count) Sektoren · \(z.total) Geräte").font(.caption).foregroundStyle(.secondary)
                                    }
                                    Spacer()
                                    if z.down > 0 { Text("\(z.down) ✕").font(.subheadline.bold()).foregroundStyle(PW.bad) }
                                }
                            }
                        }
                    } header: {
                        if let at = t.at, at > 0 { Text("Stand \(Date(ms: at).formatted(date: .omitted, time: .shortened))") }
                    }
                }
            } else if store.loading {
                ProgressView()
            }
        }
        .navigationTitle(store.tree?.map?.name ?? "Netz")
        .searchable(text: $query, prompt: "Gerät – Name oder IP")
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Toggle("Nur Orte mit Ausfällen", isOn: $onlyDown)
                    Toggle("Gekündigte ausblenden", isOn: $hideOld)
                } label: { Image(systemName: "line.3.horizontal.decrease.circle") }
            }
        }
        .onChange(of: hideOld) { _, _ in Task { await store.load(model, force: true) } }
        .refreshable { await store.load(model, force: true) }
        .task { await store.load(model) }
    }
}

struct MapZoneView: View {
    let zone: MapTreeZone
    let onlyDown: Bool

    var body: some View {
        let secs = zone.sectors.filter { !onlyDown || $0.isDown || ($0.down ?? 0) > 0 }
        List {
            ForEach(secs) { s in
                if let kids = s.kids, !kids.isEmpty {
                    NavigationLink { MapSectorView(sector: s) } label: {
                        VStack(alignment: .leading, spacing: 3) {
                            MapNodeRow(node: s)
                            Text("\(kids.count) Kunden" + ((s.down ?? 0) > 0 ? " · \(s.down ?? 0) nicht erreichbar" : ""))
                                .font(.caption).foregroundStyle((s.down ?? 0) > 0 ? PW.bad : .secondary)
                        }
                    }
                } else {
                    NavigationLink { MapNodeView(node: s) } label: { MapNodeRow(node: s) }
                }
            }
        }
        .navigationTitle(zone.name)
        .navigationBarTitleDisplayMode(.inline)
    }
}

struct MapSectorView: View {
    let sector: MapTreeNode

    var body: some View {
        List {
            Section { NavigationLink { MapNodeView(node: sector) } label: { MapNodeRow(node: sector) } } header: { Text("Sektor / Mast") }
            Section("Kunden (\(sector.kids?.count ?? 0))") {
                ForEach(sector.kids ?? []) { k in NavigationLink { MapNodeView(node: k) } label: { MapNodeRow(node: k) } }
            }
        }
        .navigationTitle(sector.name)
        .navigationBarTitleDisplayMode(.inline)
    }
}

struct MapNodeRow: View {
    let node: MapTreeNode
    var body: some View {
        HStack(spacing: 8) {
            Circle().fill(mapColor(node.st)).frame(width: 9, height: 9)
            VStack(alignment: .leading, spacing: 1) {
                Text(node.name).font(.subheadline).lineLimit(2)
                Text([node.addr ?? "", mapStatusText(node)].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(node.isDown ? PW.bad : .secondary)
            }
        }
    }
}

struct MapNodeView: View {
    let node: MapTreeNode
    var body: some View {
        List {
            Section {
                LabeledContent("Status", value: mapStatusText(node))
                if let a = node.addr, !a.isEmpty { LabeledContent("Adresse", value: a) }
                if let k = node.kids, !k.isEmpty { LabeledContent("Kunden", value: "\(k.count)") }
            }
            if let h = node.host {
                Section { NavigationLink(value: h) { Label("Überwachter Host – Verlauf und Statistik", systemImage: "chart.xyaxis.line") } }
            } else {
                Section {
                    Text("Nur Erreichbarkeit (alle 5 Minuten). Für Meldungen und Verlauf das Gerät im Web-Dashboard unter Netzkarte → „Als Host überwachen“ anlegen.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .navigationTitle(node.name)
        .navigationBarTitleDisplayMode(.inline)
    }
}
