import SwiftUI

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
