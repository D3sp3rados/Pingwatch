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
                ForEach(st.sites) { site in
                    let hosts = st.hosts.filter { $0.site == site.id }
                    if !hosts.isEmpty {
                        Section {
                            ForEach(hosts) { h in
                                NavigationLink(value: h.id) {
                                    HostRow(host: h, buckets: model.overview?.hosts[h.id] ?? [])
                                }
                            }
                        } header: {
                            SiteHeader(site: site)
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
