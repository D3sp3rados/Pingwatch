import SwiftUI
import Charts

enum TimeRange: String, CaseIterable, Identifiable {
    case m15 = "15 min", h1 = "1 h", h6 = "6 h", h24 = "24 h", d7 = "7 T", d30 = "30 T"
    var id: String { rawValue }
    var seconds: TimeInterval {
        switch self {
        case .m15: return 900
        case .h1: return 3600
        case .h6: return 6 * 3600
        case .h24: return 86400
        case .d7: return 7 * 86400
        case .d30: return 30 * 86400
        }
    }
}

struct ChartPoint: Identifiable {
    let id: Int
    let date: Date
    let agg: Agg
}

struct HostDetailView: View {
    let hostID: String
    @Environment(AppModel.self) private var model
    @State private var range: TimeRange = .h1
    @State private var series: SeriesResponse?
    @State private var events: [PWEvent] = []
    @State private var selected: ChartPoint?
    @State private var error: String?

    private var host: HostView? { model.host(hostID) }

    var body: some View {
        List {
            if let h = host {
                Section { header(h) }
            }
            Section {
                Picker("Zeitraum", selection: $range) {
                    ForEach(TimeRange.allCases) { Text($0.rawValue).tag($0) }
                }
                .pickerStyle(.segmented)
                chart
                    .frame(height: 230)
                legend
            }
            if let s = series { statsGrid(s) }
            Section("Ereignisse im Zeitraum") {
                if events.isEmpty {
                    Text("Keine Ereignisse 👍").foregroundStyle(.secondary)
                }
                ForEach(events.prefix(200)) { EventRow(event: $0, showHost: false) }
            }
            Section {
                NavigationLink("Statistik nach Tag und Uhrzeit") {
                    StatsView(hostID: hostID)
                }
            }
        }
        .navigationTitle(host?.name ?? "Host")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: range) { await load() }
        .task {
            // kurze Zeiträume laufend nachladen
            while !Task.isCancelled {
                try? await Task.sleep(for: .seconds(range.seconds <= 6 * 3600 ? 5 : 60))
                await load()
            }
        }
        .refreshable { await load() }
    }

    private func header(_ h: HostView) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                StatusDot(color: PW.color(forStatus: h.status), size: 12)
                Text(h.statusText).bold().foregroundStyle(PW.color(forStatus: h.status))
                Spacer()
                Text(h.lastR >= 0 && h.status != "down" ? fmtMs(h.lastR) : "—")
                    .font(.title2.bold().monospacedDigit())
            }
            Text("\(h.addr)\(h.method == "tcp" ? " · TCP \(h.port)" : "") · von \(model.siteName(h.site)) · alle \(Double(h.intervalMs) / 1000, format: .number) s")
                .font(.caption).foregroundStyle(.secondary)
            if h.threshold > 0 {
                Text("Spitze ab \(fmtMs(h.threshold))\(h.thresholdMs == 0 ? " (automatisch)" : "") · Ausfall ab \(h.outageCount) Pings")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if !h.lastErr.isEmpty && h.lastR < 0 {
                Text(h.lastErr).font(.caption).foregroundStyle(PW.bad)
            }
        }
    }

    private var points: [ChartPoint] {
        guard let s = series else { return [] }
        return s.b.enumerated().map { i, a in
            ChartPoint(id: i, date: Date(ms: s.from + Int64(i) * s.bucket + s.bucket / 2), agg: a)
        }
    }

    @ViewBuilder
    private var chart: some View {
        if let error {
            Text(error).foregroundStyle(.red).frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if series == nil {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            let pts = points
            Chart {
                ForEach(pts) { p in
                    if p.agg.lost > 0 {
                        RectangleMark(x: .value("Zeit", p.date), yStart: .value("", 0.0), yEnd: .value("", yMax))
                            .foregroundStyle(PW.bad.opacity(p.agg.lossRatio >= 0.5 ? 0.35 : 0.18))
                    }
                    if let lo = p.agg.minMs, let hi = p.agg.maxMs {
                        AreaMark(x: .value("Zeit", p.date), yStart: .value("Min", lo), yEnd: .value("Max", hi))
                            .foregroundStyle(PW.accent.opacity(0.18))
                            .interpolationMethod(.monotone)
                    }
                    if let avg = p.agg.avgMs {
                        LineMark(x: .value("Zeit", p.date), y: .value("Ø", avg))
                            .foregroundStyle(PW.accent)
                            .lineStyle(StrokeStyle(lineWidth: 1.6))
                            .interpolationMethod(.monotone)
                    }
                    if p.agg.spk > 0, let mx = p.agg.maxMs {
                        PointMark(x: .value("Zeit", p.date), y: .value("Max", mx))
                            .foregroundStyle(PW.warn)
                            .symbolSize(18)
                    }
                }
                if let s = series, s.threshold > 0 {
                    RuleMark(y: .value("Schwelle", Double(s.threshold) / 10))
                        .foregroundStyle(PW.warn.opacity(0.8))
                        .lineStyle(StrokeStyle(lineWidth: 1, dash: [4, 3]))
                }
                if let sel = selected {
                    RuleMark(x: .value("Auswahl", sel.date))
                        .foregroundStyle(.secondary.opacity(0.5))
                        .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                            tooltip(sel)
                        }
                }
            }
            .chartYScale(domain: 0...yMax)
            .chartYAxis {
                AxisMarks(position: .leading) { v in
                    AxisGridLine()
                    AxisValueLabel { if let d = v.as(Double.self) { Text("\(Int(d)) ms").font(.caption2) } }
                }
            }
            .chartOverlay { proxy in
                GeometryReader { geo in
                    Rectangle().fill(.clear).contentShape(Rectangle())
                        .gesture(DragGesture(minimumDistance: 0)
                            .onChanged { v in
                                guard let plot = proxy.plotFrame else { return }
                                let x = v.location.x - geo[plot].origin.x
                                if let d: Date = proxy.value(atX: x) {
                                    selected = pts.min { abs($0.date.timeIntervalSince(d)) < abs($1.date.timeIntervalSince(d)) }
                                }
                            }
                            .onEnded { _ in selected = nil })
                }
            }
        }
    }

    private var yMax: Double {
        var top = 10.0
        for p in points { if let m = p.agg.maxMs { top = max(top, m) } }
        if let s = series, s.threshold > 0 { top = max(top, Double(s.threshold) / 10 * 1.15) }
        return top * 1.1
    }

    private func tooltip(_ p: ChartPoint) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(fmtDayTime(p.date, seconds: range.seconds <= 3600)).bold()
            if p.agg.n == 0 {
                Text("keine Daten")
            } else {
                Text("Ø \(fmtMs(p.agg.avgMs)) · max \(fmtMs(p.agg.maxMs))")
                if p.agg.lost > 0 {
                    Text("\(p.agg.lost) von \(p.agg.n) verloren").foregroundStyle(PW.bad)
                }
                if p.agg.spk > 0 { Text("\(p.agg.spk) Spitzen").foregroundStyle(PW.warn) }
            }
        }
        .font(.caption2)
        .padding(6)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 8))
    }

    private var legend: some View {
        HStack(spacing: 12) {
            legendItem(PW.accent, "Ø")
            legendItem(PW.accent.opacity(0.3), "Min–Max")
            legendItem(PW.bad.opacity(0.5), "Verlust")
            legendItem(PW.warn, "Spitze")
        }
        .font(.caption2)
        .foregroundStyle(.secondary)
    }

    private func legendItem(_ c: Color, _ t: String) -> some View {
        HStack(spacing: 4) { RoundedRectangle(cornerRadius: 2).fill(c).frame(width: 12, height: 8); Text(t) }
    }

    @ViewBuilder
    private func statsGrid(_ s: SeriesResponse) -> some View {
        let total = s.b.reduce(into: (n: 0, lost: 0, spk: 0, sum: 0.0, good: 0, mn: Double.infinity, mx: 0.0)) { r, a in
            r.n += a.n; r.lost += a.lost; r.spk += a.spk
            if let avg = a.avgMs { r.sum += avg * Double(a.good); r.good += a.good }
            if let mn = a.minMs { r.mn = min(r.mn, mn) }
            if let mx = a.maxMs { r.mx = max(r.mx, mx) }
        }
        let outages = events.filter { $0.type == "outage" }
        let downtime = outages.reduce(0) { $0 + $1.duration }
        Section("Im Zeitraum") {
            LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible()), GridItem(.flexible())], spacing: 10) {
                StatCell(label: "Pings", value: "\(total.n)")
                StatCell(label: "Verloren", value: total.lost > 0 ? "\(total.lost) · \(fmtPct(Double(total.lost) / Double(max(total.n, 1))))" : "0",
                         color: total.lost > 0 ? PW.bad : nil)
                StatCell(label: "Ø", value: total.good > 0 ? fmtMs(total.sum / Double(total.good)) : "–")
                StatCell(label: "Min", value: total.mn.isFinite ? fmtMs(total.mn) : "–")
                StatCell(label: "Max", value: total.mx > 0 ? fmtMs(total.mx) : "–", color: total.spk > 0 ? PW.warn : nil)
                StatCell(label: "Spitzen", value: "\(total.spk)", color: total.spk > 0 ? PW.warn : nil)
                StatCell(label: "Ausfälle", value: "\(outages.count)", color: outages.isEmpty ? nil : PW.bad)
                StatCell(label: "Ausfallzeit", value: outages.isEmpty ? "–" : fmtDuration(downtime), color: outages.isEmpty ? nil : PW.bad)
            }
            .padding(.vertical, 4)
        }
    }

    private func load() async {
        guard let api = model.api else { return }
        let to = Date()
        let from = to.addingTimeInterval(-range.seconds)
        do {
            async let s = api.series(host: hostID, from: from, to: to, n: 240)
            async let e = api.events(host: hostID, from: from, to: to, limit: 500)
            let (ss, ee) = try await (s, e)
            series = ss
            events = ee
            error = nil
        } catch {
            if (error as? URLError)?.code != .cancelled { self.error = error.localizedDescription }
        }
    }
}

struct StatCell: View {
    let label: String
    let value: String
    var color: Color? = nil
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label).font(.caption2).foregroundStyle(.secondary)
            Text(value).font(.subheadline.bold().monospacedDigit()).foregroundStyle(color ?? .primary)
                .lineLimit(1).minimumScaleFactor(0.7)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
