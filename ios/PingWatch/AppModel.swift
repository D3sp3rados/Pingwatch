import SwiftUI
import UIKit
import AuthenticationServices
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

    /// Anmelden mit Passkey (Face ID / Touch ID) – ohne Passwort.
    func loginWithPasskey(server: String) async throws {
        guard let url = API.normalize(server) else { throw APIError.invalidServer }
        let od = try await PasskeyAPI.post(url.appendingPathComponent("passkey/login/options"), token: nil, body: [:])
        let o = try JSONDecoder().decode(PasskeyAPI.Options.self, from: od)
        guard let ch = Data(base64URL: o.challenge) else { throw APIError.invalidServer }
        let provider = ASAuthorizationPlatformPublicKeyCredentialProvider(relyingPartyIdentifier: o.relyingParty)
        let req = provider.createCredentialAssertionRequest(challenge: ch)
        req.userVerificationPreference = .required
        let auth = try await PasskeyAuthorizer().perform(req)
        guard let a = auth.credential as? ASAuthorizationPlatformPublicKeyCredentialAssertion else {
            throw APIError.http(0, "Keine Antwort vom Passkey.")
        }
        let body: [String: Any] = [
            "app": true,
            "id": a.credentialID.base64URL,
            "clientDataJSON": a.rawClientDataJSON.base64URL,
            "authenticatorData": a.rawAuthenticatorData.base64URL,
            "signature": a.signature.base64URL,
            "userHandle": a.userID.base64URL,
        ]
        let d = try await PasskeyAPI.post(url.appendingPathComponent("passkey/login"), token: nil, body: body)
        struct R: Decodable { let token: String }
        let r = try JSONDecoder().decode(R.self, from: d)
        let c = Credentials(server: url, token: r.token)
        c.save()
        creds = c
        await refresh()
        await requestPush()
        WidgetCenter.shared.reloadAllTimelines()
    }

    /// Legt für dieses iPhone einen Passkey an (Konto der aktuellen Anmeldung).
    func addPasskey() async throws {
        guard let c = creds else { throw APIError.unauthorized }
        let od = try await PasskeyAPI.post(c.server.appendingPathComponent("api/passkey/options"), token: c.token, body: ["purpose": "register"])
        let o = try JSONDecoder().decode(PasskeyAPI.Options.self, from: od)
        guard let ch = Data(base64URL: o.challenge), let u = o.user, let uid = Data(base64URL: u.id) else {
            throw APIError.invalidServer
        }
        let provider = ASAuthorizationPlatformPublicKeyCredentialProvider(relyingPartyIdentifier: o.relyingParty)
        let req = provider.createCredentialRegistrationRequest(challenge: ch, name: u.name, userID: uid)
        req.userVerificationPreference = .required
        let auth = try await PasskeyAuthorizer().perform(req)
        guard let r = auth.credential as? ASAuthorizationPlatformPublicKeyCredentialRegistration,
              let att = r.rawAttestationObject else {
            throw APIError.http(0, "Keine Antwort vom Passkey.")
        }
        let body: [String: Any] = [
            "name": UIDevice.current.model,
            "clientDataJSON": r.rawClientDataJSON.base64URL,
            "attestationObject": att.base64URL,
        ]
        _ = try await PasskeyAPI.post(c.server.appendingPathComponent("api/passkey/register"), token: c.token, body: body)
    }

    func logout() async {
        if let api, let t = deviceToken {
            try? await api.unregisterDevice(token: t)
        }
        Credentials.clear()
        API.currentOrg = nil
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

// MARK: - Passkeys

extension Data {
    init?(base64URL s: String) {
        var t = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while t.count % 4 != 0 { t += "=" }
        self.init(base64Encoded: t)
    }

    var base64URL: String {
        base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}

enum PasskeyAPI {
    struct Options: Decodable {
        struct RP: Decodable { let id: String }
        struct User: Decodable { let id: String; let name: String }
        let challenge: String
        let rpId: String?
        let rp: RP?
        let user: User?
        var relyingParty: String { rpId ?? rp?.id ?? "" }
    }

    static func post(_ url: URL, token: String?, body: [String: Any]) async throws -> Data {
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if let token { req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        req.httpBody = try JSONSerialization.data(withJSONObject: body)
        let (data, resp) = try await API.session.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        guard (200..<300).contains(code) else {
            throw APIError.http(code, String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "")
        }
        return data
    }
}

/// Führt eine Passkey-Anfrage aus (Face ID / Touch ID) und wartet auf das Ergebnis.
@MainActor
final class PasskeyAuthorizer: NSObject, ASAuthorizationControllerDelegate, ASAuthorizationControllerPresentationContextProviding {
    private var continuation: CheckedContinuation<ASAuthorization, Error>?
    private var controller: ASAuthorizationController?

    func perform(_ request: ASAuthorizationRequest) async throws -> ASAuthorization {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<ASAuthorization, Error>) in
            continuation = cont
            let c = ASAuthorizationController(authorizationRequests: [request])
            c.delegate = self
            c.presentationContextProvider = self
            controller = c
            c.performRequests()
        }
    }

    nonisolated func authorizationController(controller: ASAuthorizationController, didCompleteWithAuthorization authorization: ASAuthorization) {
        MainActor.assumeIsolated {
            continuation?.resume(returning: authorization)
            continuation = nil
        }
    }

    nonisolated func authorizationController(controller: ASAuthorizationController, didCompleteWithError error: Error) {
        MainActor.assumeIsolated {
            continuation?.resume(throwing: error)
            continuation = nil
        }
    }

    nonisolated func presentationAnchor(for controller: ASAuthorizationController) -> ASPresentationAnchor {
        MainActor.assumeIsolated {
            let windows = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }.flatMap { $0.windows }
            return windows.first(where: { $0.isKeyWindow }) ?? windows.first ?? ASPresentationAnchor()
        }
    }
}

/// Fehlertext für Passkey-Abbruch durch den Benutzer ausblenden.
func isPasskeyCancel(_ error: Error) -> Bool {
    (error as? ASAuthorizationError)?.code == .canceled
}
