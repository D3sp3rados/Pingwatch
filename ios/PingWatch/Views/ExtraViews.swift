import SwiftUI
import UIKit

// MARK: - Host pausieren / stummschalten

/// Pausieren (Prüfung aus) bzw. Meldungen stumm – für einen Host direkt in der App.
struct HostMuteSection: View {
    @Environment(AppModel.self) private var model
    let host: HostView
    @State private var busy = false
    @State private var error: String?
    @State private var ask: String?   // "pause" oder "mute"

    var body: some View {
        Section {
            if host.paused {
                Button {
                    run { try await $0.resume(scope: "host", id: host.id) }
                } label: {
                    Label("Wieder überwachen", systemImage: "play.circle").foregroundStyle(PW.ok)
                }
                if let u = host.pauseUntil, u > 0 {
                    Text("pausiert bis \(fmtDayTime(Date(ms: u)))").font(.caption).foregroundStyle(.secondary)
                }
            } else {
                Button { ask = "pause" } label: {
                    Label("Überwachung pausieren", systemImage: "pause.circle").foregroundStyle(.yellow)
                }
                if host.maint == true {
                    Label("Meldungen sind gerade stummgeschaltet", systemImage: "bell.slash").foregroundStyle(.secondary)
                    if let m = muteFor(host) {
                        Button("Stummschaltung „\(m.name)“ beenden") { run { try await $0.endMute(id: m.id) } }
                    }
                } else {
                    Button { ask = "mute" } label: {
                        Label("Nur Meldungen stummschalten", systemImage: "bell.slash")
                    }
                }
            }
            if let error { Text(error).font(.caption).foregroundStyle(PW.bad) }
        } footer: {
            Text("Pausieren: wird nicht mehr geprüft (z. B. Gerät abgebaut). Stumm: wird weiter geprüft, aber ohne Meldungen.")
        }
        .disabled(busy)
        .confirmationDialog(ask == "pause" ? "Überwachung pausieren – wie lange?" : "Meldungen stummschalten – wie lange?",
                            isPresented: Binding(get: { ask != nil }, set: { if !$0 { ask = nil } }), titleVisibility: .visible) {
            let mode = ask ?? "mute"
            ForEach(MuteDuration.all, id: \.label) { d in
                Button(d.label) {
                    run { try await $0.mute(scope: "host", id: host.id, mode: mode, until: d.until()) }
                }
            }
        }
    }

    private func muteFor(_ h: HostView) -> MuteView? {
        (model.state?.mutes ?? []).first { $0.name.contains(h.name) } ?? (model.state?.mutes ?? []).first { $0.all == true }
    }

    private func run(_ f: @escaping (API) async throws -> Void) {
        guard let api = model.api else { return }
        busy = true
        Task {
            defer { busy = false }
            do {
                try await f(api)
                error = nil
                await model.refresh(withOverview: false)
            } catch {
                self.error = error.localizedDescription
            }
        }
    }
}

struct MuteDuration {
    let label: String
    let until: () -> Int64
    static let all: [MuteDuration] = [
        MuteDuration(label: "1 Stunde") { Date().addingTimeInterval(3600).ms },
        MuteDuration(label: "4 Stunden") { Date().addingTimeInterval(4 * 3600).ms },
        MuteDuration(label: "Bis heute Abend") {
            Calendar.current.date(bySettingHour: 23, minute: 59, second: 0, of: Date())?.ms ?? Date().addingTimeInterval(3600).ms
        },
        MuteDuration(label: "Bis morgen früh") {
            let t = Calendar.current.date(byAdding: .day, value: 1, to: Date()) ?? Date()
            return Calendar.current.date(bySettingHour: 7, minute: 0, second: 0, of: t)?.ms ?? Date().addingTimeInterval(12 * 3600).ms
        },
        MuteDuration(label: "1 Woche") { Date().addingTimeInterval(7 * 86400).ms },
        MuteDuration(label: "Dauerhaft") { -1 },
    ]
}

// MARK: - Gerätestatus am Host (Fritzbox, MikroTik, Netonix)

struct HostDeviceLinks: View {
    let host: HostView

    var body: some View {
        if host.fritz == true || host.mt == true || host.nx == true {
            Section("Gerät") {
                if host.fritz == true {
                    NavigationLink { FritzInfoView(hostID: host.id, title: host.name) } label: { Label("Fritzbox – Status, WLAN, Telefonie", systemImage: "wifi.router") }
                }
                if host.mt == true {
                    NavigationLink { MikroTikInfoView(hostID: host.id, title: host.name) } label: { Label("MikroTik – Zustand", systemImage: "server.rack") }
                }
                if host.nx == true {
                    NavigationLink { NetonixInfoView(hostID: host.id, title: host.name) } label: { Label("Netonix – Ports & PoE", systemImage: "cable.connector") }
                }
            }
        }
    }
}

struct KVRow: View {
    let k: String
    let v: String
    var color: Color? = nil
    init(_ k: String, _ v: String, color: Color? = nil) { self.k = k; self.v = v; self.color = color }
    var body: some View {
        HStack(alignment: .top) {
            Text(k).foregroundStyle(.secondary)
            Spacer()
            Text(v).multilineTextAlignment(.trailing).foregroundStyle(color ?? .primary)
        }
    }
}

struct FritzInfoView: View {
    @Environment(AppModel.self) private var model
    let hostID: String
    let title: String
    @State private var info: FritzInfo?
    @State private var error: String?

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(PW.bad) }
            if let f = info {
                if let e = f.err, !e.isEmpty { Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(PW.bad) } }
                if let s = f.status {
                    Section("Internet") {
                        KVRow("Verbindung", s.conn == "Connected" ? "verbunden" : (s.conn ?? "unbekannt"), color: s.conn == "Connected" ? PW.ok : PW.bad)
                        if let ip = s.extIP { KVRow("Externe IP", ip) }
                        if let d = s.down, d > 0 { KVRow(s.dsl == true ? "Sync" : "Leitung", "↓ \(fmtBitrate(d)) · ↑ \(fmtBitrate(s.up))") }
                        if let u = s.connUptime, u > 0 { KVRow("Verbunden seit", fmtDuration(TimeInterval(u))) }
                        KVRow("Abbrüche 24 h", "\(f.drops24 ?? 0)", color: (f.drops24 ?? 0) > 0 ? PW.warn : nil)
                        if let a = f.avail, a >= 0 { KVRow("Verfügbar 24 h", String(format: "%.2f %%", a)) }
                    }
                    Section("Box") {
                        if let m = s.model { KVRow("Modell", m) }
                        if let fw = s.fw { KVRow("FRITZ!OS", fw + ((f.extra?.updAvail ?? false) ? " · Update \(f.extra?.updVersion ?? "") verfügbar" : "")) }
                        if let u = s.boxUptime, u > 0 { KVRow("Laufzeit", fmtDuration(TimeInterval(u))) }
                        if let a = s.access { KVRow("Anschluss", a.replacingOccurrences(of: "X_AVM-DE_", with: "")) }
                    }
                }
                if let x = f.extra {
                    if let v = x.voip, !v.isEmpty {
                        Section("Rufnummern") {
                            ForEach(v, id: \.number) { n in
                                let ok = ["registered", "connected", "up"].contains((n.status ?? "").lowercased())
                                KVRow(n.number, n.status == nil ? (n.registrar ?? "–") : (ok ? "angemeldet" : (n.status ?? "")), color: n.status == nil ? nil : (ok ? PW.ok : PW.bad))
                            }
                        }
                    }
                    if let d = x.defl, !d.isEmpty {
                        Section("Rufumleitungen") {
                            ForEach(Array(d.enumerated()), id: \.offset) { _, r in
                                KVRow(r.number ?? "alle", (r.on ? "aktiv → " : "aus · ") + (r.to ?? "–"), color: r.on ? PW.warn : Color.secondary)
                            }
                        }
                    }
                    if let c = x.calls, !c.isEmpty {
                        Section("Anrufe (letzte 14 Tage)") {
                            ForEach(Array(c.prefix(40).enumerated()), id: \.offset) { _, call in
                                HStack {
                                    Image(systemName: call.type == "missed" ? "phone.arrow.down.left.fill" : call.type == "out" ? "phone.arrow.up.right" : "phone.arrow.down.left")
                                        .foregroundStyle(call.type == "missed" ? PW.bad : .secondary)
                                    VStack(alignment: .leading) {
                                        Text(call.other ?? "unbekannt").font(.subheadline.bold())
                                        Text([call.date, call.name ?? "", call.device ?? ""].filter { !$0.isEmpty }.joined(separator: " · ")).font(.caption).foregroundStyle(.secondary)
                                    }
                                    Spacer()
                                    Text(call.type == "missed" ? "verpasst" : (call.dur ?? "")).font(.caption).foregroundStyle(call.type == "missed" ? PW.bad : .secondary)
                                }
                            }
                        }
                    }
                    if let w = x.wlan, !w.isEmpty {
                        Section("WLAN") {
                            ForEach(w, id: \.idx) { n in
                                KVRow((n.band ?? "#\(n.idx)") + " · " + n.ssid, n.on ? "an · \(n.clients) Clients" : "aus", color: n.on ? nil : Color.secondary)
                            }
                        }
                    }
                    if let d = x.dect, !d.isEmpty {
                        Section("DECT") {
                            ForEach(Array(d.enumerated()), id: \.offset) { _, h in
                                KVRow(h.name, [h.model ?? "", (h.update ?? false) ? "Update verfügbar" : ""].filter { !$0.isEmpty }.joined(separator: " · "))
                            }
                        }
                    }
                    if let m = x.missing, !m.isEmpty {
                        Section("Nicht abfragbar") { ForEach(m, id: \.self) { Text($0).font(.caption).foregroundStyle(.secondary) } }
                    }
                }
            } else if error == nil {
                ProgressView()
            }
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        guard let api = model.api else { return }
        do { info = try await api.fritz(host: hostID); error = nil } catch { self.error = error.localizedDescription }
    }
}

struct MikroTikInfoView: View {
    @Environment(AppModel.self) private var model
    let hostID: String
    let title: String
    @State private var info: MtInfo?
    @State private var error: String?

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(PW.bad) }
            if let i = info {
                if let e = i.err, !e.isEmpty { Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(PW.bad) } }
                if let o = i.open, !o.isEmpty {
                    Section("Aktuelle Probleme") { ForEach(o, id: \.self) { Label($0, systemImage: "exclamationmark.circle").foregroundStyle(PW.bad) } }
                }
                if let c = i.cur {
                    Section("System") {
                        let r = c.res ?? [:]
                        if let v = r["identity"] { KVRow("Name", v) }
                        if let v = r["board-name"] { KVRow("Modell", v) }
                        if let v = r["version"] { KVRow("RouterOS", v + ((c.upd?["latest-version"]).map { $0 != v ? " · Update \($0)" : "" } ?? "")) }
                        if let v = r["uptime"] { KVRow("Laufzeit", v) }
                        if let v = r["cpu-load"] { KVRow("CPU", v + " %" + (i.cpuMax24.map { String(format: " · max 24 h %.0f %%", $0) } ?? "")) }
                        KVRow("Neustarts 24 h", "\(i.boots24 ?? 0)", color: (i.boots24 ?? 0) > 0 ? PW.warn : nil)
                        if let p = c.ppp, p >= 0 { KVRow("PPP-Sitzungen", "\(p)") }
                        if let d = c.dhcp, d >= 0 { KVRow("DHCP-Leases", "\(d)") }
                        if let n = c.conns, n >= 0 { KVRow("Verbindungen", "\(n)") }
                    }
                    if let h = c.health, !h.isEmpty {
                        Section("Sensoren") { ForEach(h, id: \.n) { KVRow($0.n, $0.v) } }
                    }
                }
            } else if error == nil {
                ProgressView()
            }
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        guard let api = model.api else { return }
        do { info = try await api.mikrotik(host: hostID); error = nil } catch { self.error = error.localizedDescription }
    }
}

struct NetonixInfoView: View {
    @Environment(AppModel.self) private var model
    let hostID: String
    let title: String
    @State private var info: NxInfo?
    @State private var error: String?

    var body: some View {
        List {
            if let error { Text(error).foregroundStyle(PW.bad) }
            if let i = info {
                if let e = i.err, !e.isEmpty { Section { Label(e, systemImage: "exclamationmark.triangle").foregroundStyle(PW.bad) } }
                if let o = i.open, !o.isEmpty {
                    Section("Aktuelle Probleme") { ForEach(o, id: \.self) { Label($0, systemImage: "exclamationmark.circle").foregroundStyle(PW.bad) } }
                }
                if let c = i.cur {
                    Section("System") {
                        if let v = c.model { KVRow("Modell", v) }
                        if let v = c.fw { KVRow("Firmware", v) }
                        if let u = c.up, u > 0 { KVRow("Laufzeit", fmtDuration(TimeInterval(u))) }
                        if let v = c.cpu { KVRow("CPU", String(format: "%.0f %%", v)) }
                        if let t = c.boardT, t > 0 { KVRow("Temperatur", String(format: "%.0f °C", t)) }
                        if let v = c.v48, v > 0 { KVRow("48 V", String(format: "%.1f V", v)) }
                        let w = (c.ports ?? []).reduce(0) { $0 + $1.w }
                        if w > 0 { KVRow("PoE gesamt", String(format: "%.1f W", w)) }
                    }
                    if let p = c.ports, !p.isEmpty {
                        Section("Ports") {
                            ForEach(p) { port in
                                HStack {
                                    StatusDot(color: port.link == "Down" ? .gray : PW.ok, size: 8)
                                    Text("\(port.n)").bold().monospacedDigit()
                                    if let n = port.name, !n.isEmpty { Text(n).foregroundStyle(.secondary).lineLimit(1) }
                                    Spacer()
                                    Text(port.link).font(.caption)
                                    if port.poe != "Off" { Text(port.w > 0 ? String(format: "%@ · %.1f W", port.poe, port.w) : port.poe).font(.caption).foregroundStyle(PW.accent) }
                                }
                            }
                        }
                    }
                }
            } else if error == nil {
                ProgressView()
            }
        }
        .navigationTitle(title)
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        guard let api = model.api else { return }
        do { info = try await api.netonix(host: hostID); error = nil } catch { self.error = error.localizedDescription }
    }
}

// MARK: - UniFi: Ports, PoE-Neustart, WLAN, Clients, Neustart

struct UniFiDetailSections: View {
    @Environment(AppModel.self) private var model
    let device: UniFiDevice
    let siteID: String
    @State private var detail: UXDetail?
    @State private var error: String?
    @State private var msg: String?
    @State private var confirm: (String, Int)?   // Aktion, Port

    private var canEdit: Bool { model.state?.me?.canEdit ?? true }

    var body: some View {
        Section("Ports, WLAN & Clients") {
            if let error {
                Text(error).font(.caption).foregroundStyle(PW.bad)
                Button("Erneut versuchen") { Task { await load() } }
            } else if let d = detail {
                if let u = d.upgrade, !u.isEmpty {
                    Label("Firmware-Update verfügbar" + (u != "ja" ? " – \(u)" : ""), systemImage: "arrow.up.circle").foregroundStyle(PW.warn)
                } else {
                    KVRow("Firmware", (d.fw ?? "–") + " · aktuell")
                }
                if let u = d.uplink, !u.isEmpty { KVRow("Uplink", u) }
                if canEdit {
                    Button { confirm = ("restart", 0) } label: { Label("Gerät neu starten", systemImage: "arrow.clockwise") }
                }
                if let msg { Text(msg).font(.caption).foregroundStyle(PW.ok) }
            } else {
                HStack { Spacer(); ProgressView(); Spacer() }
            }
        }
        .task { await load() }
        .confirmationDialog(confirmText, isPresented: Binding(get: { confirm != nil }, set: { if !$0 { confirm = nil } }), titleVisibility: .visible) {
            let c = confirm
            Button(c?.0 == "poe" ? "PoE-Neustart" : "Neu starten", role: .destructive) {
                if let c { Task { await act(c.0, c.1) } }
            }
        }
        if let d = detail {
            if let ports = d.ports, !ports.isEmpty {
                Section("Ports (\(ports.filter(\.up).count)/\(ports.count) verbunden)") {
                    ForEach(ports) { p in
                        portRow(p)
                    }
                }
            }
            if let r = d.radios, !r.isEmpty {
                Section("WLAN") {
                    ForEach(r, id: \.band) { x in
                        VStack(alignment: .leading, spacing: 2) {
                            HStack {
                                Text(x.band).bold()
                                Spacer()
                                Text("\(x.clients ?? 0) Clients").font(.caption)
                            }
                            Text(radioText(x)).font(.caption).foregroundStyle((x.util ?? 0) > 70 ? PW.bad : Color.secondary)
                        }
                    }
                }
            }
            Section("Verbundene Clients (\(d.clients?.count ?? 0))") {
                ForEach((d.clients ?? []).sorted { $0.name.lowercased() < $1.name.lowercased() }) { c in
                    ClientRow(client: c, showUplink: false)
                }
                if (d.clients ?? []).isEmpty { Text("Keine Clients direkt an diesem Gerät.").foregroundStyle(.secondary) }
            }
        }
    }

    private func radioText(_ x: UXRadio) -> String {
        var l: [String] = []
        if let c = x.channel { l.append("Kanal \(c)") }
        if let w = x.width { l.append("\(w) MHz") }
        if let u = x.util { l.append(String(format: "Auslastung %.0f %%", u)) }
        if let s = x.std { l.append(s) }
        return l.joined(separator: " · ")
    }

    @ViewBuilder
    private func portRow(_ p: UXPort) -> some View {
        let poeOK = canEdit && p.poe != nil && p.poeOn == true
        HStack {
            StatusDot(color: p.up ? PW.ok : .gray, size: 8)
            Text("\(p.idx)").bold().monospacedDigit()
            if let n = p.name, !n.isEmpty { Text(n).foregroundStyle(.secondary).lineLimit(1) }
            if p.uplink == true { Text("Uplink").font(.caption2).foregroundStyle(PW.accent) }
            Spacer()
            VStack(alignment: .trailing) {
                Text(p.speedText).font(.caption.monospacedDigit())
                if p.poe != nil, p.poeOn == true {
                    Text(p.poeW.map { String(format: "PoE %.1f W", $0) } ?? "PoE an").font(.caption2).foregroundStyle(PW.accent)
                }
            }
            if poeOK {
                Button { confirm = ("poe", p.idx) } label: { Image(systemName: "bolt.circle") }
                    .buttonStyle(.borderless)
                    .accessibilityLabel("PoE-Neustart Port \(p.idx)")
            }
        }
    }

    private var confirmText: String {
        guard let c = confirm else { return "" }
        if c.0 == "poe" { return "Strom an Port \(c.1) von \(device.name) kurz aus- und wieder einschalten? Das angeschlossene Gerät startet neu." }
        return "\(device.name) neu starten? Das Gerät ist 1–3 Minuten nicht erreichbar" + (device.cat == "gateway" ? " – am Gateway fällt das Internet am Standort aus." : ".")
    }

    private func load() async {
        guard let api = model.api, let id = device.devID else { return }
        do {
            detail = try await api.unifiDevice(site: siteID, id: id, usite: device.usite ?? "", mac: device.mac)
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func act(_ action: String, _ port: Int) async {
        guard let api = model.api, let id = device.devID else { return }
        do {
            msg = try await api.unifiAction(site: siteID, usite: device.usite ?? "", id: id, name: device.name, action: action, port: port)
        } catch {
            msg = nil
            self.error = error.localizedDescription
        }
    }
}

struct ClientRow: View {
    let client: UXClient
    var showUplink = true
    var body: some View {
        HStack {
            Image(systemName: client.type == "WIRELESS" ? "wifi" : "cable.connector").foregroundStyle(.secondary).frame(width: 20)
            VStack(alignment: .leading, spacing: 1) {
                Text(client.name).font(.subheadline).lineLimit(1)
                Text([client.ip ?? "", showUplink ? (client.uplink ?? "") : ""].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer()
            if let s = client.since { Text(fmtDayTime(Date(ms: s))).font(.caption2).foregroundStyle(.secondary) }
        }
    }
}

/// Alle Clients eines UniFi-Standorts mit Suche.
struct UniFiClientsView: View {
    @Environment(AppModel.self) private var model
    let siteID: String
    let title: String
    @State private var clients: [UXClient]?
    @State private var error: String?
    @State private var query = ""

    var body: some View {
        let list = (clients ?? []).filter { query.isEmpty || [$0.name, $0.ip ?? "", $0.mac ?? "", $0.uplink ?? ""].joined(separator: " ").localizedCaseInsensitiveContains(query) }
        List {
            if let error { Text(error).foregroundStyle(PW.bad) }
            if let c = clients {
                Section("\(c.count) Clients · \(c.filter { $0.type == "WIRELESS" }.count) WLAN") {
                    ForEach(list.sorted { ($0.uplink ?? "", $0.name.lowercased()) < ($1.uplink ?? "", $1.name.lowercased()) }) { ClientRow(client: $0) }
                }
            } else if error == nil {
                ProgressView()
            }
        }
        .searchable(text: $query, prompt: "Name, IP, MAC, Gerät")
        .navigationTitle("Clients · " + title)
        .navigationBarTitleDisplayMode(.inline)
        .task { await load() }
        .refreshable { await load() }
    }

    private func load() async {
        guard let api = model.api else { return }
        do { clients = try await api.unifiClients(site: siteID); error = nil } catch { self.error = error.localizedDescription }
    }
}

// MARK: - Protect: Vorschaubild

struct CameraSnapshotSection: View {
    @Environment(AppModel.self) private var model
    let siteID: String
    let cameraID: String
    @State private var image: UIImage?
    @State private var error: String?
    @State private var loading = false

    var body: some View {
        Section {
            if let image {
                Image(uiImage: image).resizable().scaledToFit().clipShape(RoundedRectangle(cornerRadius: 8))
            } else if let error {
                Text(error).font(.caption).foregroundStyle(PW.bad)
            } else {
                HStack { Spacer(); ProgressView(); Spacer() }.frame(height: 160)
            }
            Button { Task { await load() } } label: { Label("Neu laden", systemImage: "arrow.clockwise") }.disabled(loading)
        } header: {
            Text("Vorschaubild")
        }
        .task { await load() }
    }

    private func load() async {
        guard let api = model.api else { return }
        loading = true
        defer { loading = false }
        do {
            let d = try await api.snapshot(site: siteID, camera: cameraID)
            if let img = UIImage(data: d) { image = img; error = nil } else { error = "Kein Bild erhalten" }
        } catch {
            self.error = error.localizedDescription
        }
    }
}

// MARK: - Konto wechseln

struct OrgSwitcher: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let list = model.state?.orgs ?? model.state?.myOrgs ?? []
        if list.count > 1 {
            Menu {
                ForEach(list) { o in
                    Button {
                        API.currentOrg = o.id
                        NotificationCenter.default.post(name: .pwOrgChanged, object: nil)
                    } label: {
                        if o.id == model.state?.org?.id { Label(o.name, systemImage: "checkmark") } else { Text(o.name) }
                    }
                }
            } label: {
                Label(model.state?.org?.name ?? "Konto", systemImage: "building.2")
            }
        }
    }
}

// MARK: - Rechner (RMM)

struct MachinesView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        let ms = (model.state?.machines ?? []).sorted { ($0.status == "bad" ? 0 : $0.status == "warn" ? 1 : 2, $0.name.lowercased()) < ($1.status == "bad" ? 0 : $1.status == "warn" ? 1 : 2, $1.name.lowercased()) }
        List {
            ForEach(ms) { m in
                NavigationLink { MachineDetailView(machineID: m.id) } label: { MachineRow(machine: m) }
            }
            if ms.isEmpty { Text("Keine Rechner überwacht.").foregroundStyle(.secondary) }
        }
        .navigationTitle("Rechner")
        .refreshable { await model.refresh(withOverview: false) }
    }
}

struct MachineRow: View {
    let machine: MachineView
    var color: Color { machine.status == "ok" ? PW.ok : machine.status == "warn" ? PW.warn : machine.status == "bad" || machine.status == "off" ? PW.bad : .gray }
    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack {
                StatusDot(color: color, size: 8)
                Text(machine.name).font(.subheadline.bold()).lineLimit(1)
                Spacer()
                Text(machine.statusText).font(.caption).foregroundStyle(color)
            }
            Text([machine.cpu.map { $0 >= 0 ? "CPU \($0) %" : "" } ?? "", machine.mem.map { $0 >= 0 ? "RAM \($0) %" : "" } ?? "", machine.disk.map { $0 >= 0 ? "\(machine.diskName ?? "Disk") \($0) %" : "" } ?? "", (machine.problems ?? []).first ?? ""]
                .filter { !$0.isEmpty }.joined(separator: " · "))
                .font(.caption).foregroundStyle(.secondary).lineLimit(1)
        }
    }
}

struct MachineDetailView: View {
    @Environment(AppModel.self) private var model
    let machineID: String
    @State private var confirm = false
    @State private var code = ""
    @State private var msg: String?
    @State private var busy = false

    var body: some View {
        let m = model.state?.machines?.first { $0.id == machineID }
        List {
            if let m {
                Section {
                    KVRow("Status", m.statusText)
                    if let h = m.host, !h.isEmpty { KVRow("Rechnername", h) }
                    KVRow("Standort", model.siteName(m.site))
                    if let d = m.desc, !d.isEmpty { KVRow("Beschreibung", d) }
                    if let c = m.cpu, c >= 0 { KVRow("CPU", "\(c) %") }
                    if let r = m.mem, r >= 0 { KVRow("RAM", "\(r) %") }
                    if let d = m.disk, d >= 0 { KVRow(m.diskName ?? "Festplatte", "\(d) % belegt") }
                    if let n = m.svcN, n > 0 { KVRow("Dienste", "\(m.svcOK ?? 0)/\(n) laufen") }
                    if let r = m.rebootSince, r > 0 { KVRow("Neustart ausstehend seit", fmtDayTime(Date(ms: r)), color: PW.warn) }
                }
                if let p = m.problems, !p.isEmpty {
                    Section("Probleme") { ForEach(p, id: \.self) { Label($0, systemImage: "exclamationmark.triangle").foregroundStyle(PW.bad) } }
                }
                if model.state?.me?.canEdit ?? true {
                    Section {
                        Button(role: .destructive) { confirm = true } label: { Label("Rechner neu starten", systemImage: "power") }
                            .disabled(busy || m.online != true)
                        TextField("2FA-Code (falls verlangt)", text: $code).keyboardType(.numberPad)
                        if let msg { Text(msg).font(.caption) }
                    } footer: {
                        Text("Startet den Rechner sofort neu. Bei Konten mit Signaturschlüssel geht das nur im Dashboard im Browser.")
                    }
                }
            }
        }
        .navigationTitle(m?.name ?? "Rechner")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("\(m?.name ?? "Rechner") jetzt neu starten?", isPresented: $confirm, titleVisibility: .visible) {
            Button("Neu starten", role: .destructive) { Task { await reboot() } }
        }
    }

    private func reboot() async {
        guard let api = model.api else { return }
        busy = true
        defer { busy = false }
        do {
            try await api.rebootMachine(id: machineID, code: code)
            msg = "Neustart in Auftrag gegeben – der Agent führt ihn gleich aus."
        } catch {
            msg = error.localizedDescription
        }
    }
}
