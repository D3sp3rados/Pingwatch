import SwiftUI

struct SettingsView: View {
    @Environment(AppModel.self) private var model
    @State private var testResult: String?
    @State private var confirmLogout = false

    var body: some View {
        @Bindable var m = model
        Form {
            Section {
                Toggle("Ausfall & wieder erreichbar", isOn: $m.notifyOutage)
                Toggle("Paketverlust", isOn: $m.notifyLoss)
                Toggle("Latenzspitzen", isOn: $m.notifySpike)
                Toggle("Standort offline / online", isOn: $m.notifyOffline)
            } header: {
                Text("Benachrichtigungen")
            } footer: {
                Text("Paketverlust und Spitzen höchstens alle 10 Minuten pro Host.")
            }

            Section {
                NavigationLink {
                    HostFilterView()
                } label: {
                    LabeledContent("Hosts", value: model.notifyHosts.isEmpty ? "Alle" : "\(model.notifyHosts.count) ausgewählt")
                }
            }

            Section("Status") {
                if model.state?.push == false {
                    Label("Push ist auf dem Server noch nicht eingerichtet (APNs-Schlüssel fehlt).", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange)
                }
                LabeledContent("Gerät registriert", value: model.deviceToken == nil ? "nein" : "ja")
                if let s = model.pushStatus {
                    Text(s).font(.caption).foregroundStyle(.red)
                }
                Button("Benachrichtigungen erneut anfordern") { Task { await model.requestPush() } }
                Button("Testnachricht senden") {
                    Task {
                        guard let api = model.api else { return }
                        do { testResult = try await api.pushTest() } catch { testResult = error.localizedDescription }
                    }
                }
                .disabled(model.deviceToken == nil || model.state?.push != true)
                if let testResult { Text(testResult).font(.caption) }
            }

            Section("Server") {
                if let c = model.creds {
                    LabeledContent("Adresse", value: c.server.host() ?? c.server.absoluteString)
                }
                if let v = model.state?.version { LabeledContent("Server-Version", value: v) }
                if let d = model.lastUpdate { LabeledContent("Aktualisiert", value: fmtTime(d, seconds: true)) }
                if let c = model.creds {
                    Link("Dashboard im Browser öffnen", destination: c.server)
                }
            }

            Section {
                Button("Abmelden", role: .destructive) { confirmLogout = true }
            }
        }
        .navigationTitle("Einstellungen")
        .onChange(of: [model.notifyOutage, model.notifyLoss, model.notifySpike, model.notifyOffline]) { _, _ in
            Task { await model.syncPushPrefs() }
        }
        .confirmationDialog("Abmelden? Dieses iPhone bekommt dann keine Benachrichtigungen mehr.",
                            isPresented: $confirmLogout, titleVisibility: .visible) {
            Button("Abmelden", role: .destructive) { Task { await model.logout() } }
        }
    }
}

struct HostFilterView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        List {
            Section {
                Button {
                    model.notifyHosts = []
                    Task { await model.syncPushPrefs() }
                } label: {
                    HStack {
                        Text("Alle Hosts").foregroundStyle(.primary)
                        Spacer()
                        if model.notifyHosts.isEmpty { Image(systemName: "checkmark") }
                    }
                }
            } footer: {
                Text("Oder einzelne Hosts auswählen – dann gibt es nur für diese Benachrichtigungen.")
            }
            Section {
                ForEach(model.state?.hosts ?? []) { h in
                    Button {
                        if let i = model.notifyHosts.firstIndex(of: h.id) {
                            model.notifyHosts.remove(at: i)
                        } else {
                            model.notifyHosts.append(h.id)
                        }
                        Task { await model.syncPushPrefs() }
                    } label: {
                        HStack {
                            StatusDot(color: PW.color(forStatus: h.status), size: 8)
                            Text(h.name).foregroundStyle(.primary)
                            Spacer()
                            if model.notifyHosts.contains(h.id) { Image(systemName: "checkmark") }
                        }
                    }
                }
            }
        }
        .navigationTitle("Hosts")
    }
}
