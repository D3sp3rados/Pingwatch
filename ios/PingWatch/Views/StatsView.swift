import SwiftUI

/// Statistik: Zeilen = Tage, Spalten = Stunden. Farbe nach Paketverlust oder Antwortzeit.
struct StatsView: View {
    let hostID: String
    @Environment(AppModel.self) private var model
    @State private var stats: [DayStat] = []
    @State private var days = 14
    @State private var metric = 0 // 0 Verlust, 1 Ø, 2 Spitzen
    @State private var selected: (day: DayStat, hour: Int)?

    var body: some View {
        List {
            Section {
                Picker("Anzeige", selection: $metric) {
                    Text("Verlust").tag(0); Text("Ø Ping").tag(1); Text("Spitzen").tag(2)
                }
                .pickerStyle(.segmented)
                Picker("Tage", selection: $days) {
                    Text("7").tag(7); Text("14").tag(14); Text("30").tag(30)
                }
                .pickerStyle(.segmented)
            }
            Section {
                ScrollView(.horizontal, showsIndicators: false) {
                    Grid(horizontalSpacing: 2, verticalSpacing: 2) {
                        GridRow {
                            Text("")
                            ForEach(0..<24, id: \.self) { h in
                                Text(h % 3 == 0 ? String(format: "%02d", h) : "").font(.system(size: 9)).foregroundStyle(.secondary)
                            }
                        }
                        ForEach(stats) { d in
                            GridRow {
                                Text(dayLabel(d.day)).font(.caption2.bold()).frame(width: 56, alignment: .leading)
                                ForEach(0..<24, id: \.self) { h in
                                    RoundedRectangle(cornerRadius: 3)
                                        .fill(cellColor(d.hours[h]))
                                        .frame(width: 14, height: 18)
                                        .onTapGesture { selected = (d, h) }
                                }
                            }
                        }
                    }
                    .padding(.vertical, 4)
                }
                if let s = selected {
                    let a = s.day.hours[s.hour]
                    VStack(alignment: .leading, spacing: 2) {
                        Text("\(dayLabel(s.day.day)) \(String(format: "%02d:00–%02d:00", s.hour, s.hour + 1))").bold()
                        if a.n == 0 {
                            Text("keine Daten").foregroundStyle(.secondary)
                        } else {
                            Text("\(a.n) Pings · \(a.lost) verloren (\(fmtPct(a.lossRatio)))")
                            Text("Ø \(fmtMs(a.avgMs)) · min \(fmtMs(a.minMs)) · max \(fmtMs(a.maxMs)) · \(a.spk) Spitzen")
                        }
                    }
                    .font(.caption)
                }
            } footer: {
                Text("Feld antippen für Details. Grau = keine Daten.")
            }
            Section("Pro Tag") {
                ForEach(stats) { d in
                    HStack {
                        Text(dayLabel(d.day)).frame(width: 70, alignment: .leading)
                        Text(d.total.n > 0 ? fmtPct(d.total.lossRatio) : "–")
                            .foregroundStyle(d.total.lost > 0 ? PW.bad : .secondary)
                            .frame(width: 70, alignment: .leading)
                        Text(fmtMs(d.total.avgMs)).frame(width: 60, alignment: .leading)
                        Spacer()
                        if d.outages > 0 {
                            Text("\(d.outages)× Ausfall · \(fmtDuration(TimeInterval(d.downtime) / 1000))")
                                .foregroundStyle(PW.bad)
                        }
                    }
                    .font(.caption.monospacedDigit())
                }
            }
        }
        .navigationTitle("Statistik")
        .task(id: days) {
            guard let api = model.api else { return }
            if let r = try? await api.stats(host: hostID, days: days) { stats = r.days }
        }
    }

    private var medianAvg: Double {
        let v = stats.flatMap { $0.hours.compactMap(\.avgMs) }.sorted()
        return v.isEmpty ? 0 : v[v.count / 2]
    }

    private func cellColor(_ a: Agg) -> Color {
        if a.n == 0 { return Color.gray.opacity(0.2) }
        switch metric {
        case 0:
            let p = a.lossRatio * 100
            if a.lost == 0 { return PW.ok.opacity(0.45) }
            if p >= 5 { return PW.bad }
            if p >= 1 { return PW.bad.opacity(0.7) }
            return PW.loss.opacity(p >= 0.2 ? 0.75 : 0.45)
        case 1:
            guard let v = a.avgMs, medianAvg > 0 else { return PW.bad }
            let r = v / medianAvg
            if r <= 1.3 { return PW.ok.opacity(0.45) }
            if r <= 2 { return PW.warn.opacity(0.45) }
            if r <= 4 { return PW.warn.opacity(0.85) }
            return PW.bad.opacity(0.75)
        default:
            if a.spk == 0 { return PW.ok.opacity(0.45) }
            return PW.warn.opacity(min(1, 0.35 + Double(a.spk) * 0.04))
        }
    }

    private func dayLabel(_ day: String) -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd"
        guard let d = f.date(from: day) else { return day }
        let cal = Calendar.current
        if cal.isDateInToday(d) { return "Heute" }
        if cal.isDateInYesterday(d) { return "Gestern" }
        let o = DateFormatter()
        o.locale = Locale(identifier: "de_DE")
        o.dateFormat = "EE dd.MM."
        return o.string(from: d)
    }
}
