import WidgetKit
import SwiftUI

struct StatusEntry: TimelineEntry {
    let date: Date
    let total: Int
    let ok: Int
    let down: [String]
    let warn: [String]
    let error: String?
}

struct Provider: TimelineProvider {
    func placeholder(in context: Context) -> StatusEntry {
        StatusEntry(date: Date(), total: 6, ok: 5, down: ["Antenne"], warn: [], error: nil)
    }

    func getSnapshot(in context: Context, completion: @escaping (StatusEntry) -> Void) {
        if context.isPreview {
            completion(placeholder(in: context))
            return
        }
        Task { completion(await load()) }
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<StatusEntry>) -> Void) {
        Task {
            let e = await load()
            // iOS begrenzt Widget-Aktualisierungen; alle 5 Minuten anfragen
            completion(Timeline(entries: [e], policy: .after(Date().addingTimeInterval(300))))
        }
    }

    private func load() async -> StatusEntry {
        guard let c = Credentials.load() else {
            return StatusEntry(date: Date(), total: 0, ok: 0, down: [], warn: [], error: "In der App anmelden")
        }
        do {
            let st = try await API(creds: c).state()
            let active = st.hosts.filter { !$0.paused }
            let down = active.filter { $0.status == "down" }.map(\.name)
            let warn = active.filter { $0.status == "loss" || $0.status == "spike" }.map(\.name)
            let ok = active.filter { $0.status == "ok" }.count
            return StatusEntry(date: Date(), total: active.count, ok: ok, down: down, warn: warn, error: nil)
        } catch {
            return StatusEntry(date: Date(), total: 0, ok: 0, down: [], warn: [], error: "Server nicht erreichbar")
        }
    }
}

struct PingWatchWidgetView: View {
    let entry: StatusEntry
    @Environment(\.widgetFamily) private var family

    private var color: Color {
        if entry.error != nil { return .gray }
        if !entry.down.isEmpty { return PW.bad }
        if !entry.warn.isEmpty { return PW.warn }
        return PW.ok
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Image(systemName: "waveform.path.ecg").foregroundStyle(PW.accent)
                Text("PingWacht").font(.caption.bold())
                Spacer()
                StatusDot(color: color, size: 10)
            }
            if let err = entry.error {
                Spacer()
                Text(err).font(.caption).foregroundStyle(.secondary)
                Spacer()
            } else {
                Text("\(entry.ok)/\(entry.total)")
                    .font(.system(size: family == .systemSmall ? 34 : 30, weight: .bold, design: .rounded))
                    .foregroundStyle(color)
                    .minimumScaleFactor(0.6)
                Text(entry.down.isEmpty && entry.warn.isEmpty ? "alle erreichbar" : "OK")
                    .font(.caption).foregroundStyle(.secondary)
                if !entry.down.isEmpty {
                    Text(entry.down.joined(separator: ", "))
                        .font(.caption.bold()).foregroundStyle(PW.bad)
                        .lineLimit(family == .systemSmall ? 2 : 3)
                }
                if !entry.warn.isEmpty && family != .systemSmall {
                    Text("Störung: " + entry.warn.joined(separator: ", "))
                        .font(.caption).foregroundStyle(PW.warn).lineLimit(2)
                }
                Spacer(minLength: 0)
                Text("Stand \(fmtTime(entry.date))").font(.caption2).foregroundStyle(.tertiary)
            }
        }
        .containerBackground(.fill.tertiary, for: .widget)
    }
}

@main
struct PingWatchWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "PingWatchStatus", provider: Provider()) { entry in
            PingWatchWidgetView(entry: entry)
        }
        .configurationDisplayName("PingWacht")
        .description("Wie viele Hosts erreichbar sind und welche gerade ausfallen.")
        .supportedFamilies([.systemSmall, .systemMedium])
    }
}
