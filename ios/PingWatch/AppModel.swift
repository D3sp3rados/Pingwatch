import SwiftUI
import UserNotifications
import WidgetKit

@MainActor
@Observable
final class AppModel {
    var creds: Credentials? = Credentials.load()
    var state: StateResponse?
    var overview: OverviewResponse?
    var lastError: String?
    var lastUpdate: Date?
    var loading = false

    /// Aus einer Benachrichtigung angetippter Host
    var openHostID: String?

    // Push-Einstellungen (lokal gemerkt, an den Server übertragen)
    var deviceToken: String? = UserDefaults.standard.string(forKey: "apnsToken")
    var notifyOutage = UserDefaults.standard.object(forKey: "nOutage") as? Bool ?? true
    var notifyLoss = UserDefaults.standard.object(forKey: "nLoss") as? Bool ?? false
    var notifySpike = UserDefaults.standard.object(forKey: "nSpike") as? Bool ?? false
    var notifyOffline = UserDefaults.standard.object(forKey: "nOffline") as? Bool ?? true
    var notifyHosts: [String] = UserDefaults.standard.stringArray(forKey: "nHosts") ?? []
    var pushStatus: String?

    var api: API? { creds.map { API(creds: $0) } }

    func login(server: String, email: String, password: String) async throws {
        guard let url = API.normalize(server) else { throw APIError.invalidServer }
        let c = try await API.login(server: url, email: email, password: password)
        c.save()
        creds = c
        await refresh()
        await requestPush()
        WidgetCenter.shared.reloadAllTimelines()
    }

    func logout() async {
        if let api, let t = deviceToken {
            try? await api.unregisterDevice(token: t)
        }
        Credentials.clear()
        creds = nil
        state = nil
        overview = nil
        WidgetCenter.shared.reloadAllTimelines()
    }

    func refresh(withOverview: Bool = true) async {
        guard let api else { return }
        loading = true
        defer { loading = false }
        do {
            async let s = api.state()
            if withOverview {
                async let o = api.overview(hours: 24, n: 48)
                let (st, ov) = try await (s, o)
                state = st
                overview = ov
            } else {
                state = try await s
            }
            lastError = nil
            lastUpdate = Date()
        } catch APIError.unauthorized {
            lastError = APIError.unauthorized.localizedDescription
            Credentials.clear()
            creds = nil
        } catch {
            if (error as? URLError)?.code != .cancelled {
                lastError = error.localizedDescription
            }
        }
    }

    func host(_ id: String) -> HostView? { state?.hosts.first { $0.id == id } }

    func siteName(_ id: String) -> String {
        if id == "cloud" { return "Cloud" }
        return state?.sites.first { $0.id == id }?.name ?? id
    }

    // MARK: Push

    func requestPush() async {
        let center = UNUserNotificationCenter.current()
        do {
            let ok = try await center.requestAuthorization(options: [.alert, .sound, .badge])
            if ok {
                UIApplication.shared.registerForRemoteNotifications()
            } else {
                pushStatus = "Benachrichtigungen sind in den iOS-Einstellungen ausgeschaltet."
            }
        } catch {
            pushStatus = error.localizedDescription
        }
    }

    func didRegister(token: String) {
        deviceToken = token
        UserDefaults.standard.set(token, forKey: "apnsToken")
        Task { await syncPushPrefs() }
    }

    func syncPushPrefs() async {
        let d = UserDefaults.standard
        d.set(notifyOutage, forKey: "nOutage")
        d.set(notifyLoss, forKey: "nLoss")
        d.set(notifySpike, forKey: "nSpike")
        d.set(notifyOffline, forKey: "nOffline")
        d.set(notifyHosts, forKey: "nHosts")
        guard let api, let token = deviceToken else { return }
        #if DEBUG
        let sandbox = true
        #else
        let sandbox = false
        #endif
        let prefs = DevicePrefs(token: token, name: UIDevice.current.name, sandbox: sandbox,
                                outage: notifyOutage, loss: notifyLoss, spike: notifySpike,
                                offline: notifyOffline, hosts: notifyHosts)
        do {
            try await api.registerDevice(prefs)
            pushStatus = nil
        } catch {
            pushStatus = "Registrierung fehlgeschlagen: \(error.localizedDescription)"
        }
    }
}
