import SwiftUI

struct OverviewView: View {
    @Environment(AppModel.self) private var model
    @AppStorage("ovMode") private var mode = "group"
    @State private var statusList: String?

    var body: some View {
        List {
            if let err = model.lastError {
                Section {
                    Label(err, systemImage: "wifi.exclamationmark").foregroundStyle(.red)
                }
            }
            if let st = model.state {
                summary(st)
                if st.hosts.isEmpty {
                    ContentUnavailableView("Noch keine Hosts", systemImage: "network",
                                           description: Text("Hosts legst du im Dashboard unter Einstellungen an."))
                }
                let groups = st.groups ?? []
                if !groups.isEmpty {
                    Section {
                        Picker("Ansicht", selection: $mode) {
                            Text("Gruppen").tag("group")
                            Text("Standorte").tag("site")
                        }
                        .pickerStyle(.segmented)
                    }
                    .listRowBackground(Color.clear)
                    .listRowInsets(EdgeInsets())
                }
                if !groups.isEmpty && mode == "group" {
                    Section {
                        ForEach(pwFolders(st)) { f in
                            NavigationLink {
                                FolderView(folderID: f.id)
                            } label: {
                                FolderRow(folder: f)
                            }
                        }
                    }
                } else {
                    ForEach(st.sites) { site in
                        let hosts = st.hosts.filter { $0.site == site.id }
                        if !hosts.isEmpty || site.unifi?.on == true {
                            Section {
                                ForEach(hosts) { h in
                                    NavigationLink(value: h.id) {
                                        HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                                    }
                                }
                                if let u = site.unifi, u.on {
                                    UniFiSection(summary: u, siteName: site.name, siteID: site.id)
                                }
                            } header: {
                                SiteHeader(site: site)
                            }
                        }
                    }
                }
            } else if model.lastError == nil {
                HStack { Spacer(); ProgressView(); Spacer() }.listRowBackground(Color.clear)
            }
        }
        .navigationTitle("PingWacht")
        .refreshable { await model.refresh() }
        .navigationDestination(item: $statusList) { k in StatusListView(kind: k) }
    }

    @ViewBuilder
    private func summary(_ st: StateResponse) -> some View {
        let active = st.hosts.filter { !$0.paused }
        let down = active.filter { $0.status == "down" }.count
        let warn = active.filter { $0.status == "loss" || $0.status == "spike" }.count
        let nodata = active.filter { ["stale", "siteoff", "unknown"].contains($0.status) }.count
        let ok = active.count - down - warn - nodata
        Section {
            HStack(spacing: 12) {
                Button { statusList = "ok" } label: {
                    SummaryTile(value: "\(ok)/\(active.count)", label: "OK", color: PW.ok)
                }
                Button { statusList = "down" } label: {
                    SummaryTile(value: "\(down)", label: "ohne Antwort", color: down > 0 ? PW.bad : .secondary)
                }
                Button { statusList = "warn" } label: {
                    SummaryTile(value: "\(warn)", label: "Störung", color: warn > 0 ? PW.warn : .secondary)
                }
            }
            .buttonStyle(.plain)
            .listRowInsets(EdgeInsets(top: 8, leading: 0, bottom: 8, trailing: 0))
            .listRowBackground(Color.clear)
        }
    }
}

/// Liste der Hosts hinter einer Kachel oben (OK / ohne Antwort / Störung).
struct StatusListView: View {
    @Environment(AppModel.self) private var model
    let kind: String

    private var title: String {
        switch kind {
        case "down": return "Ohne Antwort"
        case "warn": return "Störung"
        default: return "OK"
        }
    }

    private func match(_ h: HostView) -> Bool {
        if h.paused { return false }
        switch kind {
        case "down": return h.status == "down"
        case "warn": return h.status == "loss" || h.status == "spike"
        default: return !["down", "loss", "spike", "stale", "siteoff", "unknown"].contains(h.status)
        }
    }

    var body: some View {
        let hosts = (model.state?.hosts ?? []).filter(match)
            .sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
        List {
            if hosts.isEmpty {
                ContentUnavailableView(kind == "ok" ? "Keine Hosts" : "Alles in Ordnung",
                                       systemImage: kind == "ok" ? "network" : "checkmark.circle")
            }
            ForEach(hosts) { h in
                NavigationLink(value: h.id) {
                    HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                }
            }
        }
        .navigationTitle("\(title) (\(hosts.count))")
        .refreshable { await model.refresh() }
    }
}

private struct SummaryTile: View {
    let value: String
    let label: String
    let color: Color
    var body: some View {
        VStack(spacing: 2) {
            Text(value).font(.title2.bold().monospacedDigit()).foregroundStyle(color)
            Text(label).font(.caption).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 10)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: 12))
        .contentShape(RoundedRectangle(cornerRadius: 12))
    }
}

private struct SiteHeader: View {
    let site: SiteView
    var body: some View {
        HStack(spacing: 6) {
            StatusDot(color: site.online ? PW.ok : PW.bad, size: 8)
            Text(site.name)
            if site.id != "cloud" && !site.online {
                Text(site.lastSeen > 0 ? "· offline seit \(fmtDayTime(Date(ms: site.lastSeen)))" : "· nie verbunden")
                    .textCase(nil)
            }
        }
    }
}

struct HostRow: View {
    let host: HostView
    let buckets: [Agg]

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .firstTextBaseline) {
                StatusDot(color: PW.color(forStatus: host.status))
                VStack(alignment: .leading, spacing: 1) {
                    Text(host.name).font(.headline).lineLimit(1)
                    Text(host.addr + (host.method == "tcp" ? " · TCP \(host.port)" : ""))
                        .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer()
                VStack(alignment: .trailing, spacing: 1) {
                    Text(host.status == "down" || host.lastR < 0 || host.status == "siteoff" || host.status == "stale" ? "—" : fmtMs(host.lastR))
                        .font(.title3.bold().monospacedDigit())
                    Text(host.status == "down" ? "\(host.statusText) (\(host.consec)×)" : host.statusText)
                        .font(.caption2.bold())
                        .foregroundStyle(PW.color(forStatus: host.status))
                }
            }
            if !buckets.isEmpty {
                TimelineBar(buckets: buckets, height: 12)
            }
            HStack {
                Label("Ø \(fmtMs(host.h1.avgMs))", systemImage: "clock")
                Spacer()
                Text("Verlust 24 h: \(fmtPct(host.h24.lossRatio))")
                    .foregroundStyle(host.h24.lost > 0 ? PW.loss : .secondary)
            }
            .font(.caption)
            .foregroundStyle(.secondary)
            if let live = host.ongoing?.first(where: { $0.type == "outage" }) {
                Text("Ausfall seit \(fmtDayTime(live.startDate, seconds: true)) (\(fmtDuration(live.duration)))")
                    .font(.caption.bold()).foregroundStyle(PW.bad)
            } else if let last = host.lastOutage {
                Text("Letzter Ausfall: \(fmtDayTime(last.startDate)) · \(fmtDuration(last.duration))")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .padding(.vertical, 4)
    }
}

/// UniFi-Block eines Standorts in der Übersicht (aufklappbar).
struct UniFiSection: View {
    let summary: UniFiSummary
    let siteName: String
    var siteID: String = ""
    @State private var expanded: Bool?

    private var devices: [UniFiDevice] { summary.devices ?? [] }
    private var hasProblem: Bool { summary.offline > 0 || (summary.wanDown ?? 0) > 0 }

    var body: some View {
        DisclosureGroup(isExpanded: Binding(get: { expanded ?? hasProblem }, set: { expanded = $0 })) {
            if let gw = devices.first(where: { $0.cat == "gateway" && !($0.wan ?? []).isEmpty }) {
                HStack(spacing: 6) {
                    ForEach(gw.wan ?? [], id: \.name) { w in
                        WANPill(wan: w)
                    }
                }
            }
            ForEach(devices) { d in
                NavigationLink {
                    UniFiDeviceView(device: d, siteName: siteName, siteID: siteID)
                } label: {
                    UniFiDeviceRow(device: d)
                }
            }
        } label: {
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Image(systemName: "wifi.router").foregroundStyle(PW.accent)
                    Text("UniFi").bold()
                    Text("\(summary.total) Geräte").foregroundStyle(.secondary)
                }
                HStack(spacing: 8) {
                    if let err = summary.err, !err.isEmpty {
                        Text("Konsole nicht erreichbar").foregroundStyle(PW.bad)
                    } else if summary.offline > 0 {
                        Text("\(summary.offline) offline").foregroundStyle(PW.bad).bold()
                    } else if summary.at != nil {
                        Text("alle online").foregroundStyle(PW.ok)
                    } else {
                        Text("warte auf Daten …").foregroundStyle(.secondary)
                    }
                    if let b = summary.boots, b > 0 {
                        Text("\(b)× Neustart").foregroundStyle(PW.warn)
                    }
                    if let w = summary.wanDown, w > 0 {
                        Text("WAN getrennt").foregroundStyle(PW.bad)
                    }
                }
                .font(.caption)
            }
        }
    }

}

struct WANPill: View {
    let wan: UniFiWAN
    var body: some View {
        HStack(spacing: 4) {
            StatusDot(color: wan.up ? PW.ok : PW.bad, size: 7)
            Text(wan.name).font(.caption.bold())
            if wan.up, let l = wan.latency, l > 0 {
                Text("\(Int(l.rounded())) ms").font(.caption.monospacedDigit()).foregroundStyle(.secondary)
            } else if !wan.up {
                Text("getrennt").font(.caption).foregroundStyle(PW.bad)
            }
        }
        .padding(.horizontal, 8).padding(.vertical, 3)
        .overlay(Capsule().stroke(Color.gray.opacity(0.3)))
    }
}

struct UniFiDeviceRow: View {
    let device: UniFiDevice
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                StatusDot(color: device.paused ? .yellow : device.isOffline ? PW.bad : (device.state == "ONLINE" ? PW.ok : PW.warn), size: 8)
                Text(device.name).font(.subheadline.bold()).lineLimit(1)
                Spacer()
                if device.avail >= 0 {
                    Text(String(format: "%.1f %%", device.avail)).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                }
            }
            HStack(spacing: 8) {
                Text(device.model)
                if device.pingAvg >= 0 { Text(String(format: "%.1f ms", device.pingAvg)) }
                if device.loss > 0 { Text(String(format: "%.0f %% Verlust", device.loss)).foregroundStyle(PW.loss) }
                if !device.boots.isEmpty { Text("\(device.boots.count)× Neustart").foregroundStyle(PW.warn) }
                if device.paused { Text("pausiert").foregroundStyle(.yellow) }
                else if device.isOffline { Text("offline seit \(fmtTime(Date(ms: device.since)))").foregroundStyle(PW.bad) }
            }
            .font(.caption).foregroundStyle(.secondary).lineLimit(1)
            UniFiBar(levels: device.bar)
        }
        .padding(.vertical, 2)
    }
}

struct UniFiBar: View {
    let levels: [Int]
    var body: some View {
        HStack(spacing: 1) {
            ForEach(Array(levels.enumerated()), id: \.offset) { _, l in
                RoundedRectangle(cornerRadius: 1)
                    .fill(color(l))
            }
        }
        .frame(height: 6)
    }
    private func color(_ l: Int) -> Color {
        switch l {
        case 1: return PW.ok
        case 2: return PW.warn
        case 3: return PW.bad
        default: return Color.gray.opacity(0.25)
        }
    }
}

struct UniFiDeviceView: View {
    let device: UniFiDevice
    let siteName: String
    var siteID: String = ""

    var body: some View {
        List {
            if !siteID.isEmpty, let k = device.key, !k.isEmpty {
                PauseSection(siteID: siteID, key: k, paused: device.paused)
            }
            Section {
                row("Status", device.paused ? "pausiert" : device.isOffline ? "offline" : device.state == "ONLINE" ? "online" : device.state.lowercased())
                row("seit", fmtDayTime(Date(ms: device.since)))
                row("Typ", device.catName)
                row("Modell", device.model)
                row("IP", device.ip.isEmpty ? "–" : device.ip)
                row("MAC", device.mac.isEmpty ? "–" : device.mac)
                if let fw = device.fw, !fw.isEmpty { row("Firmware", fw) }
            }
            Section("Zustand") {
                if let up = device.uptime, up > 0 { row("Laufzeit", fmtDuration(TimeInterval(up))) }
                if let c = device.cpu, let m = device.mem, c > 0 || m > 0 { row("CPU / RAM", String(format: "%.0f %% / %.0f %%", c, m)) }
                row("Ping (1 h)", device.pingAvg >= 0 ? String(format: "Ø %.1f ms · max %.1f ms", device.pingAvg, device.pingMax) : "–")
                row("Verlust (1 h)", String(format: "%.0f %%", device.loss))
                row("Verfügbar (24 h)", device.avail >= 0 ? String(format: "%.2f %%", device.avail) : "–")
                VStack(alignment: .leading, spacing: 4) {
                    Text("Letzte 24 h").font(.caption).foregroundStyle(.secondary)
                    UniFiBar(levels: device.bar).frame(height: 10)
                }
            }
            Section("Neustarts (24 h)") {
                if device.boots.isEmpty {
                    Text("keine").foregroundStyle(.secondary)
                } else {
                    ForEach(device.boots, id: \.self) { b in
                        Label(fmtDayTime(Date(ms: b)), systemImage: "arrow.clockwise").foregroundStyle(PW.warn)
                    }
                }
            }
            if let wans = device.wan, !wans.isEmpty {
                Section("Internet") {
                    ForEach(wans, id: \.name) { w in
                        VStack(alignment: .leading, spacing: 2) {
                            HStack {
                                StatusDot(color: w.up ? PW.ok : PW.bad, size: 8)
                                Text(w.name + ((w.active ?? false) ? " (aktiv)" : "")).bold()
                                Spacer()
                                Text(w.up ? "verbunden" : "getrennt").foregroundStyle(w.up ? PW.ok : PW.bad)
                            }
                            HStack(spacing: 8) {
                                if let ip = w.ip, !ip.isEmpty { Text(ip) }
                                if let l = w.latency, l > 0 { Text(String(format: "%.1f ms", l)) }
                                if let l = w.lat24, l > 0 { Text(String(format: "Ø 24 h %.1f ms", l)) }
                            }
                            .font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }
            }
        }
        .navigationTitle(device.name)
        .navigationBarTitleDisplayMode(.inline)
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack {
            Text(k).foregroundStyle(.secondary)
            Spacer()
            Text(v).multilineTextAlignment(.trailing)
        }
    }
}


// MARK: - Gruppen zum Durchklicken (Ordner → Inhalt → UniFi mit Auswahl)

struct PWFolder: Identifiable {
    let id: String          // Gruppen-ID, "-" = ohne Gruppe, "u:<Standort>" = nur UniFi
    let name: String
    let hosts: [HostView]
    let sites: [SiteView]   // Standorte mit UniFi, die zu den Hosts gehören
    var unifiOnly: Bool { id.hasPrefix("u:") }

    var problems: Int {
        var n = hosts.filter { !$0.paused && $0.status == "down" }.count
        for s in sites {
            n += s.unifi?.offline ?? 0
            n += (s.unifi?.protect?.devs ?? []).filter { $0.problem }.count
            if let e = s.unifi?.err, !e.isEmpty { n += 1 }
        }
        return n
    }
    var warnings: Int {
        var n = hosts.filter { !$0.paused && ($0.status == "loss" || $0.status == "spike") }.count
        for s in sites { n += (s.unifi?.protect?.devs ?? []).filter { $0.lowBattery }.count }
        return n
    }
}

func pwFolders(_ st: StateResponse) -> [PWFolder] {
    let groups = st.groups ?? []
    var used = Set<String>()
    func make(_ id: String, _ name: String, _ hosts: [HostView]) -> PWFolder {
        let ids = Set(hosts.map { $0.site })
        let sites = st.sites.filter { ids.contains($0.id) && $0.unifi?.on == true }
        sites.forEach { used.insert($0.id) }
        return PWFolder(id: id, name: name, hosts: hosts, sites: sites)
    }
    var out: [PWFolder] = []
    for g in groups {
        let hs = st.hosts.filter { $0.group == g.id }
        if !hs.isEmpty { out.append(make(g.id, g.name, hs)) }
    }
    let rest = st.hosts.filter { h in !groups.contains { $0.id == h.group } }
    if !rest.isEmpty { out.append(make("-", "Ohne Gruppe", rest)) }
    for s in st.sites where s.unifi?.on == true && !used.contains(s.id) {
        out.append(PWFolder(id: "u:" + s.id, name: "UniFi · " + s.name, hosts: [], sites: [s]))
    }
    return out
}

struct FolderRow: View {
    let folder: PWFolder

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                StatusDot(color: folder.problems > 0 ? PW.bad : folder.warnings > 0 ? PW.warn : PW.ok)
                Image(systemName: folder.unifiOnly ? "wifi.router" : "folder.fill").foregroundStyle(PW.accent)
                Text(folder.name).font(.headline).lineLimit(1)
                Spacer()
                if folder.problems > 0 {
                    Text("\(folder.problems) Störung\(folder.problems == 1 ? "" : "en")")
                        .font(.caption.bold()).foregroundStyle(PW.bad)
                } else if folder.warnings > 0 {
                    Text("\(folder.warnings) Warnung\(folder.warnings == 1 ? "" : "en")")
                        .font(.caption.bold()).foregroundStyle(PW.warn)
                }
            }
            Text(counts).font(.caption).foregroundStyle(.secondary)
            if !folder.hosts.isEmpty {
                Text(folder.hosts.prefix(5).map { $0.name }.joined(separator: " · ") + (folder.hosts.count > 5 ? " · +\(folder.hosts.count - 5)" : ""))
                    .font(.caption2).foregroundStyle(.secondary).lineLimit(1)
            }
        }
        .padding(.vertical, 4)
    }

    private var counts: String {
        var l: [String] = []
        if !folder.hosts.isEmpty { l.append("\(folder.hosts.count) Host\(folder.hosts.count == 1 ? "" : "s")") }
        let net = folder.sites.reduce(0) { $0 + ($1.unifi?.devices?.count ?? 0) }
        let prot = folder.sites.flatMap { $0.unifi?.protect?.devs ?? [] }
        if net > 0 { l.append("\(net) UniFi") }
        let cams = prot.filter { !$0.isSensor }.count, sens = prot.filter { $0.isSensor }.count
        if cams > 0 { l.append("\(cams) Protect") }
        if sens > 0 { l.append("\(sens) Sensor\(sens == 1 ? "" : "en")") }
        return l.isEmpty ? "leer" : l.joined(separator: " · ")
    }
}

struct FolderView: View {
    @Environment(AppModel.self) private var model
    let folderID: String

    var body: some View {
        let folder = model.state.flatMap { st in pwFolders(st).first { $0.id == folderID } }
        List {
            if let f = folder {
                if !f.hosts.isEmpty {
                    Section("Hosts") {
                        ForEach(f.hosts) { h in
                            NavigationLink(value: h.id) {
                                HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                            }
                        }
                    }
                }
                if !f.sites.isEmpty {
                    Section("UniFi") {
                        ForEach(f.sites) { s in
                            NavigationLink {
                                UniFiSiteView(siteID: s.id)
                            } label: {
                                UniFiSiteRow(site: s)
                            }
                        }
                    }
                }
            } else {
                ContentUnavailableView("Gruppe nicht gefunden", systemImage: "folder")
            }
        }
        .navigationTitle(folder?.name ?? "Gruppe")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await model.refresh() }
    }
}

struct UniFiSiteRow: View {
    let site: SiteView

    var body: some View {
        let u = site.unifi
        let devs = u?.devices ?? []
        let prot = u?.protect?.devs ?? []
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                Image(systemName: "wifi.router").foregroundStyle(PW.accent)
                Text(site.name).font(.headline)
                Spacer()
                let off = (u?.offline ?? 0) + prot.filter { $0.problem }.count
                if let e = u?.err, !e.isEmpty {
                    Text("nicht erreichbar").font(.caption.bold()).foregroundStyle(PW.bad)
                } else if off > 0 {
                    Text("\(off) offline").font(.caption.bold()).foregroundStyle(PW.bad)
                } else {
                    Text("alle online").font(.caption).foregroundStyle(PW.ok)
                }
            }
            Text(summary(devs, prot)).font(.caption).foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
    }

    private func summary(_ devs: [UniFiDevice], _ prot: [ProtectDev]) -> String {
        var l: [String] = []
        for (cat, name) in [("gateway", "Gateway"), ("switch", "Switches"), ("ap", "APs"), ("other", "Sonstige")] {
            let n = devs.filter { $0.cat == cat }.count
            if n > 0 { l.append("\(name) \(n)") }
        }
        let cams = prot.filter { !$0.isSensor }.count, sens = prot.filter { $0.isSensor }.count
        if cams > 0 { l.append("Protect \(cams)") }
        if sens > 0 { l.append("Sensoren \(sens)") }
        return l.joined(separator: " · ")
    }
}

/// UniFi eines Standorts mit Auswahl: Alle, Gateway, Switches, APs, Protect, Sensoren.
struct UniFiSiteView: View {
    @Environment(AppModel.self) private var model
    let siteID: String
    @State private var cat = "all"

    /// Kategorien mit Anzahl (in eigener Funktion, damit der Compiler schnell bleibt).
    static func categories(_ devs: [UniFiDevice], _ prot: [ProtectDev]) -> [(String, String, Int)] {
        var all: [(String, String, Int)] = []
        all.append(("all", "Alle", devs.count + prot.count))
        all.append(("gateway", "Gateway", devs.filter { $0.cat == "gateway" }.count))
        all.append(("switch", "Switches", devs.filter { $0.cat == "switch" }.count))
        all.append(("ap", "APs", devs.filter { $0.cat == "ap" }.count))
        all.append(("other", "Sonstige", devs.filter { $0.cat == "other" }.count))
        all.append(("protect", "Protect", prot.filter { !$0.isSensor }.count))
        all.append(("sensor", "Sensoren", prot.filter { $0.isSensor }.count))
        return all.filter { $0.0 == "all" || $0.2 > 0 }
    }

    var body: some View {
        let site = model.state?.sites.first { $0.id == siteID }
        let devs = site?.unifi?.devices ?? []
        let prot = site?.unifi?.protect?.devs ?? []
        let cats = Self.categories(devs, prot)
        let shownDevs = devs.filter { cat == "all" || $0.cat == cat }
        let shownProt = prot.filter { cat == "all" || (cat == "sensor" && $0.isSensor) || (cat == "protect" && !$0.isSensor) }
        List {
            Section {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(cats, id: \.0) { c in
                            Button {
                                cat = c.0
                            } label: {
                                Text("\(c.1) \(c.2)")
                                    .font(.subheadline.weight(cat == c.0 ? .semibold : .regular))
                                    .padding(.horizontal, 12).padding(.vertical, 6)
                                    .background(cat == c.0 ? PW.accent.opacity(0.2) : Color.gray.opacity(0.12), in: Capsule())
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
            }
            .listRowBackground(Color.clear)
            .listRowInsets(EdgeInsets(top: 4, leading: 0, bottom: 4, trailing: 0))
            if let e = site?.unifi?.err, !e.isEmpty {
                Section { Label(e, systemImage: "exclamationmark.triangle.fill").foregroundStyle(PW.bad) }
            }
            if !shownDevs.isEmpty {
                Section("Netzwerk") {
                    ForEach(shownDevs) { d in
                        NavigationLink {
                            UniFiDeviceView(device: d, siteName: site?.name ?? "", siteID: siteID)
                        } label: {
                            UniFiDeviceRow(device: d)
                        }
                    }
                }
            }
            if !shownProt.isEmpty {
                Section("UniFi Protect") {
                    ForEach(shownProt) { d in
                        NavigationLink {
                            ProtectDeviceView(device: d, siteName: site?.name ?? "", siteID: siteID)
                        } label: {
                            ProtectRow(device: d)
                        }
                    }
                }
            }
            if shownDevs.isEmpty && shownProt.isEmpty {
                Text("Keine Geräte in dieser Auswahl.").foregroundStyle(.secondary)
            }
        }
        .navigationTitle("UniFi · " + (site?.name ?? ""))
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await model.refresh() }
    }
}

struct ProtectRow: View {
    let device: ProtectDev

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                StatusDot(color: device.paused ? .yellow : device.online ? (device.lowBattery ? PW.warn : PW.ok) : PW.bad, size: 8)
                Image(systemName: device.symbol).foregroundStyle(.secondary)
                Text(device.name).font(.subheadline.bold()).lineLimit(1)
                Spacer()
                Text(device.kindName).font(.caption).foregroundStyle(.secondary)
            }
            let v = device.valuesText
            Text(device.paused ? "pausiert" + (device.online ? "" : " · offline") : device.online ? (v.isEmpty ? (device.model ?? "online") : v) : "offline")
                .font(.caption)
                .foregroundStyle(device.paused ? Color.yellow : device.online ? Color.secondary : PW.bad)
                .lineLimit(1)
        }
        .padding(.vertical, 2)
    }
}

struct ProtectDeviceView: View {
    let device: ProtectDev
    let siteName: String
    var siteID: String = ""

    var body: some View {
        List {
            if !siteID.isEmpty {
                PauseSection(siteID: siteID, key: device.pauseKey, paused: device.paused)
            }
            Section {
                row("Status", device.paused ? "pausiert" + (device.online ? "" : " (offline)") : device.online ? "online" : "offline")
                row("Art", device.kindName)
                if let m = device.model, !m.isEmpty { row("Modell", m) }
                row("Standort", siteName)
            }
            Section("Werte") {
                if let b = device.bat, b >= 0 { row("Akku", "\(b) %" + (device.lowBattery ? " (schwach)" : "")) }
                if let t = device.temp { row("Temperatur", String(format: "%.1f °C", t)) }
                if let h = device.hum { row("Luftfeuchte", "\(Int(h.rounded())) %") }
                if let l = device.lux { row("Helligkeit", "\(Int(l.rounded())) lx") }
                if let o = device.open { row("Tür/Fenster", o ? "offen" : "zu") }
                if device.leak == true { row("Wasser", "erkannt") }
                ForEach((device.vals ?? [:]).sorted(by: { $0.key < $1.key }), id: \.key) { kv in
                    row(kv.key.uppercased(), String(format: "%.0f", kv.value))
                }
            }
        }
        .navigationTitle(device.name)
        .navigationBarTitleDisplayMode(.inline)
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack {
            Text(k).foregroundStyle(.secondary)
            Spacer()
            Text(v).multilineTextAlignment(.trailing)
        }
    }
}

/// Pausieren / wieder überwachen für UniFi- und Protect-Geräte.
struct PauseSection: View {
    @Environment(AppModel.self) private var model
    let siteID: String
    let key: String
    @State var paused: Bool
    @State private var busy = false
    @State private var error: String?

    init(siteID: String, key: String, paused: Bool) {
        self.siteID = siteID
        self.key = key
        _paused = State(initialValue: paused)
    }

    var body: some View {
        Section {
            Button {
                Task { await toggle() }
            } label: {
                Label(paused ? "Wieder überwachen" : "Pausieren", systemImage: paused ? "play.circle" : "pause.circle")
                    .foregroundStyle(paused ? PW.ok : .yellow)
            }
            .disabled(busy || key.isEmpty)
            if let error { Text(error).font(.caption).foregroundStyle(PW.bad) }
        } footer: {
            Text(paused ? "Pausiert: bleibt sichtbar (gelb), aber keine Meldungen und zählt nicht als Störung." : "Pausieren, wenn das Gerät absichtlich aus ist – es bleibt sichtbar (gelb), aber ohne Meldungen.")
        }
    }

    private func toggle() async {
        guard let api = model.api else { return }
        busy = true
        defer { busy = false }
        do {
            try await api.setUniFiPause(site: siteID, key: key, on: !paused)
            paused.toggle()
            error = nil
            await model.refresh(withOverview: false)
        } catch {
            self.error = error.localizedDescription
        }
    }
}
