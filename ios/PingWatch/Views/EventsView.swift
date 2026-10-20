import SwiftUI

struct EventRow: View {
    let event: PWEvent
    var showHost = true

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            VStack(alignment: .leading, spacing: 1) {
                Text(fmtTime(event.startDate, seconds: true)).font(.subheadline.bold().monospacedDigit())
                Text(fmtDay(event.startDate)).font(.caption).foregroundStyle(.secondary)
            }
            .frame(width: 74, alignment: .leading)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(event.typeName)
                        .font(.caption2.bold())
                        .padding(.horizontal, 6).padding(.vertical, 2)
                        .background(PW.color(forEvent: event.type).opacity(0.18), in: RoundedRectangle(cornerRadius: 5))
                        .foregroundStyle(PW.color(forEvent: event.type))
                    if showHost { Text(event.name).font(.subheadline.bold()).lineLimit(1) }
                }
                Text(event.detail).font(.caption).foregroundStyle(.secondary)
                if let by = event.ackBy, event.isAcked {
                    Label("quittiert: \(by)", systemImage: "checkmark.circle.fill").font(.caption2).foregroundStyle(PW.ok)
                } else if let c = event.cause, !c.isEmpty {
                    Text("wegen \(c)").font(.caption2).foregroundStyle(.secondary)
                } else if event.maint == true {
                    Text("Wartung").font(.caption2).foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 4)
            VStack(alignment: .trailing, spacing: 1) {
                if event.isOngoing {
                    Text("läuft").font(.subheadline.bold()).foregroundStyle(PW.bad)
                    Text("seit \(fmtDuration(event.duration))").font(.caption).foregroundStyle(.secondary)
                } else if event.duration >= 1 || event.type == "outage" {
                    Text(fmtDuration(event.duration)).font(.subheadline.bold().monospacedDigit())
                    if let e = event.endDate { Text("bis \(fmtTime(e))").font(.caption).foregroundStyle(.secondary) }
                }
            }
        }
        .padding(.vertical, 2)
    }
}

struct EventsView: View {
    let hostID: String?
    @Environment(AppModel.self) private var model
    @State private var events: [PWEvent] = []
    @State private var types: Set<String> = ["outage", "loss", "spike", "offline", "device", "port"]
    @State private var days = 7
    @State private var host: String = ""
    @State private var loaded = false

    private let allTypes = [("outage", "Ausfälle"), ("loss", "Verlust"), ("spike", "Spitzen"), ("offline", "Offline"), ("device", "UniFi"), ("port", "Ports")]

    var body: some View {
        List {
            Section {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack {
                        ForEach(allTypes, id: \.0) { t in
                            let on = types.contains(t.0)
                            Button {
                                if on { types.remove(t.0) } else { types.insert(t.0) }
                            } label: {
                                Text(t.1).font(.subheadline)
                                    .padding(.horizontal, 10).padding(.vertical, 5)
                                    .background(on ? PW.color(forEvent: t.0).opacity(0.2) : Color.gray.opacity(0.12), in: Capsule())
                                    .foregroundStyle(on ? PW.color(forEvent: t.0) : .secondary)
                            }
                            .buttonStyle(.plain)
                        }
                    }
                }
                Picker("Zeitraum", selection: $days) {
                    Text("24 h").tag(1); Text("7 Tage").tag(7); Text("30 Tage").tag(30)
                }
                .pickerStyle(.segmented)
                if hostID == nil, let hosts = model.state?.hosts, !hosts.isEmpty {
                    Picker("Host", selection: $host) {
                        Text("Alle Hosts").tag("")
                        ForEach(hosts) { Text($0.name).tag($0.id) }
                    }
                }
            }
            Section(events.isEmpty ? "" : "\(events.count) Ereignisse") {
                if loaded && events.isEmpty {
                    Text("Keine Ereignisse im gewählten Zeitraum 👍").foregroundStyle(.secondary)
                }
                ForEach(events) { e in
                    Group {
                        if e.host.isEmpty {
                            EventRow(event: e)
                        } else {
                            NavigationLink(value: e.host) { EventRow(event: e) }
                        }
                    }
                    .swipeActions(edge: .leading) {
                        if e.canAck {
                            Button { Task { await ack(e) } } label: { Label("Quittieren", systemImage: "checkmark.circle") }
                                .tint(PW.ok)
                        }
                    }
                }
            }
        }
        .navigationTitle("Ereignisse")
        .task(id: "\(types.sorted())\(days)\(host)") { await load() }
        .refreshable { await load() }
    }

    private func ack(_ e: PWEvent) async {
        guard let api = model.api else { return }
        try? await api.ack(event: e.id)
        await load()
        await model.refresh()
    }

    private func load() async {
        guard let api = model.api else { return }
        let h = hostID ?? (host.isEmpty ? nil : host)
        if types.isEmpty { events = []; loaded = true; return }
        do {
            events = try await api.events(host: h, types: Array(types),
                                          from: Date().addingTimeInterval(-Double(days) * 86400), limit: 1000)
            loaded = true
        } catch {}
    }
}
