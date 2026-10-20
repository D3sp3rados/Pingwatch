import SwiftUI

struct OverviewView: View {
    @Environment(AppModel.self) private var model

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
                    ForEach(groups) { g in
                        let hosts = st.hosts.filter { $0.group == g.id }
                        if !hosts.isEmpty {
                            Section {
                                ForEach(hosts) { h in
                                    NavigationLink(value: h.id) {
                                        HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                                    }
                                }
                            } header: {
                                Label(g.name, systemImage: "folder")
                            }
                        }
                    }
                    let rest = st.hosts.filter { h in !groups.contains { $0.id == h.group } }
                    if !rest.isEmpty {
                        Section("Ohne Gruppe") {
                            ForEach(rest) { h in
                                NavigationLink(value: h.id) {
                                    HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                                }
                            }
                        }
                    }
                    ForEach(st.sites.filter { $0.unifi?.on == true }) { site in
                        Section {
                            if let u = site.unifi { UniFiSection(summary: u, siteName: site.name) }
                        } header: {
                            SiteHeader(site: site)
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
                                UniFiSection(summary: u, siteName: site.name)
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
        .navigationTitle("PingWatch")
        .refreshable { await model.refresh() }
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
                SummaryTile(value: "\(ok)/\(active.count)", label: "OK", color: PW.ok)
                SummaryTile(value: "\(down)", label: "ohne Antwort", color: down > 0 ? PW.bad : .secondary)
                SummaryTile(value: "\(warn)", label: "Störung", color: warn > 0 ? PW.warn : .secondary)
            }
            .listRowInsets(EdgeInsets(top: 8, leading: 0, bottom: 8, trailing: 0))
            .listRowBackground(Color.clear)
        }
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
                    UniFiDeviceView(device: d, siteName: siteName)
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
                StatusDot(color: device.isOffline ? PW.bad : (device.state == "ONLINE" ? PW.ok : PW.warn), size: 8)
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
                if device.isOffline { Text("offline seit \(fmtTime(Date(ms: device.since)))").foregroundStyle(PW.bad) }
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

    var body: some View {
        List {
            Section {
                row("Status", device.isOffline ? "offline" : device.state == "ONLINE" ? "online" : device.state.lowercased())
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
