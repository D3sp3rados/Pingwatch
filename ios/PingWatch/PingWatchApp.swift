import SwiftUI
import UserNotifications

final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    var model: AppModel?

    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        UNUserNotificationCenter.current().delegate = self
        return true
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        let token = deviceToken.map { String(format: "%02x", $0) }.joined()
        Task { @MainActor in self.model?.didRegister(token: token) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        Task { @MainActor in self.model?.pushStatus = "Push nicht verfügbar: \(error.localizedDescription)" }
    }

    // Benachrichtigung auch anzeigen, wenn die App offen ist
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        Task { @MainActor in await self.model?.refresh() }
        return [.banner, .sound, .list]
    }

    // Antippen öffnet den betroffenen Host
    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let host = response.notification.request.content.userInfo["host"] as? String
        await MainActor.run {
            if let host, !host.isEmpty { self.model?.openHostID = host }
        }
    }
}

@main
struct PingWatchApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @State private var model = AppModel()
    @Environment(\.scenePhase) private var phase

    var body: some Scene {
        WindowGroup {
            Group {
                if model.creds == nil {
                    LoginView()
                } else {
                    MainView()
                }
            }
            .environment(model)
            .tint(PW.accent)
            .onAppear {
                delegate.model = model
                if model.creds != nil {
                    Task { await model.requestPush() }
                }
            }
            .onChange(of: phase) { _, p in
                if p == .active { Task { await model.refresh() } }
            }
        }
    }
}
