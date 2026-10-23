import Foundation

// MARK: - Gespeicherte Zugangsdaten (App Group, damit das Widget sie mitlesen kann)

enum AppGroup {
    static let id = "group.de.rst.pingwatch"
    static var defaults: UserDefaults { UserDefaults(suiteName: id) ?? .standard }
}

struct Credentials: Codable, Equatable {
    var server: URL
    var token: String

    static func load() -> Credentials? {
        guard let d = AppGroup.defaults.data(forKey: "credentials") else { return nil }
        return try? JSONDecoder().decode(Credentials.self, from: d)
    }

    func save() {
        if let d = try? JSONEncoder().encode(self) {
            AppGroup.defaults.set(d, forKey: "credentials")
        }
    }

    static func clear() {
        AppGroup.defaults.removeObject(forKey: "credentials")
    }
}

// MARK: - Datenmodelle (entsprechen der Server-API, Zeiten in Zehntel-ms)

/// Zusammenfassung eines Zeitabschnitts. Der Server sendet `0` (keine Daten)
/// oder `[n, verloren, spitzen, avg, min, max]`.
struct Agg: Decodable, Hashable {
    var n = 0, lost = 0, spk = 0, avg = -1, min = -1, max = -1

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let a = try? c.decode([Int].self), a.count >= 6 {
            n = a[0]; lost = a[1]; spk = a[2]; avg = a[3]; min = a[4]; max = a[5]
        }
    }

    var good: Int { n - lost }
    var lossRatio: Double { n > 0 ? Double(lost) / Double(n) : 0 }
    var avgMs: Double? { avg >= 0 && good > 0 ? Double(avg) / 10 : nil }
    var minMs: Double? { min >= 0 && good > 0 ? Double(min) / 10 : nil }
    var maxMs: Double? { max >= 0 && good > 0 ? Double(max) / 10 : nil }
}

struct PWEvent: Decodable, Identifiable, Hashable {
    let id: Int64
    let host: String
    let name: String
    let site: String
    let type: String
    let start: Int64
    let end: Int64
    let last: Int64
    let count: Int
    let max: Int
    let sum: Int64
    var ackBy: String?
    var ackAt: Int64?
    var cause: String?
    var maint: Bool?

    var isAcked: Bool { (ackAt ?? 0) > 0 }
    var canAck: Bool { end == 0 && !isAcked && (type == "outage" || type == "offline" || type == "device" || type == "port") }

    var startDate: Date { Date(ms: start) }
    var endDate: Date? { end > 0 ? Date(ms: end) : nil }
    var isOngoing: Bool { end == 0 && (type == "outage" || type == "offline" || type == "device" || type == "port") }

    var typeName: String {
        switch type {
        case "outage": return "Ausfall"
        case "loss": return "Paketverlust"
        case "spike": return "Latenzspitze"
        case "offline": return "Standort offline"
        case "device": return "UniFi-Gerät"
        case "port": return "Port down"
        case "reboot": return "Neustart"
        case "wan": return "WAN getrennt"
        default: return type
        }
    }

    var detail: String {
        switch type {
        case "outage": return "\(count) Pings ohne Antwort"
        case "loss": return "\(count)× keine Antwort"
        case "spike":
            let avg = count > 0 ? Int(sum) / count : 0
            return "\(count) \(count == 1 ? "Spitze" : "Spitzen") · max \(fmtMs(max)) · Ø \(fmtMs(avg))"
        case "offline": return "Agent hat sich nicht gemeldet"
        case "device": return "offline in der UniFi-Konsole"
        case "port": return "Schnittstelle down"
        case "reboot": return "UniFi-Gerät neu gestartet"
        case "wan": return "Internetleitung getrennt"
        default: return ""
        }
    }

    var duration: TimeInterval {
        let e = end > 0 ? end : (isOngoing ? Int64(Date().timeIntervalSince1970 * 1000) : last)
        return TimeInterval(e - start) / 1000
    }
}

struct HostView: Decodable, Identifiable, Hashable {
    let id: String
    let name: String
    let addr: String
    let site: String
    let method: String
    let port: Int
    let intervalMs: Int
    let thresholdMs: Int
    let outageCount: Int
    let paused: Bool
    let ip: String
    let status: String
    let lastR: Int
    let lastT: Int64
    let lastErr: String
    let consec: Int
    let baseline: Int
    let threshold: Int
    let h1: Agg
    let h24: Agg
    let spark: [Int]
    let ongoing: [PWEvent]?
    let lastOutage: PWEvent?
    var group: String?

    var statusText: String {
        switch status {
        case "ok": return "OK"
        case "spike": return "Latenzspitze"
        case "loss": return "Paketverlust"
        case "down": return "Keine Antwort"
        case "stale": return "Keine Daten"
        case "siteoff": return "Standort offline"
        case "paused": return "Pausiert"
        default: return "Warte auf Daten"
        }
    }

    var isProblem: Bool { status == "down" || status == "loss" || status == "spike" }
}

struct SiteView: Decodable, Identifiable, Hashable {
    let id: String
    let name: String
    let online: Bool
    let lastSeen: Int64
    let ip: String
    let version: String
    let hosts: Int
    var unifi: UniFiSummary?
}

struct UniFiWAN: Decodable, Hashable {
    let name: String
    let up: Bool
    var ip: String?
    var latency: Double?
    var avail: Double?
    var active: Bool?
    var lat24: Double?
}

struct UniFiDevice: Decodable, Hashable, Identifiable {
    let name: String
    let model: String
    let mac: String
    let ip: String
    let state: String
    let since: Int64
    let cat: String
    var fw: String?
    var uptime: Int64?
    var cpu: Double?
    var mem: Double?
    var tx: Double?
    var rx: Double?
    let pingAvg: Double
    let pingMax: Double
    let loss: Double
    let avail: Double
    let boots: [Int64]
    let bar: [Int]
    var wan: [UniFiWAN]?

    var id: String { mac.isEmpty ? name : mac }
    var isOffline: Bool { ["OFFLINE", "CONNECTION_INTERRUPTED", "ISOLATED", "DISCONNECTED"].contains(state) }
    var catName: String {
        switch cat {
        case "gateway": return "Gateway"
        case "switch": return "Switch"
        case "ap": return "Access Point"
        default: return "Sonstiges"
        }
    }
}

struct UniFiSummary: Decodable, Hashable {
    let on: Bool
    var at: Int64?
    var err: String?
    let total: Int
    let offline: Int
    var boots: Int?
    var wanDown: Int?
    var devices: [UniFiDevice]?
    var protect: ProtectSummary?
}

/// UniFi Protect: Kameras, Sensoren, Lichter … (nur Status)
struct ProtectDev: Decodable, Hashable, Identifiable {
    let id: String
    let kind: String
    let name: String
    var model: String?
    var state: String?
    var bat: Int?
    var batLow: Bool?
    var temp: Double?
    var hum: Double?
    var lux: Double?
    var open: Bool?
    var leak: Bool?
    var vals: [String: Double]?

    var online: Bool { (state ?? "").isEmpty || state == "CONNECTED" }
    var lowBattery: Bool { (batLow ?? false) || ((bat ?? -1) >= 0 && (bat ?? -1) <= 15) }
    var isSensor: Bool { kind == "sensor" }
    var kindName: String {
        switch kind {
        case "camera": return "Kamera"
        case "sensor": return "Sensor"
        case "light": return "Licht"
        case "chime": return "Gong"
        case "viewer": return "Viewer"
        case "nvr": return "NVR"
        default: return kind
        }
    }
    var symbol: String {
        switch kind {
        case "camera": return "video"
        case "sensor": return "sensor"
        case "light": return "lightbulb"
        case "chime": return "bell"
        case "viewer": return "tv"
        case "nvr": return "externaldrive"
        default: return "questionmark.circle"
        }
    }
    /// Messwerte kurz: „23,5 °C · 41 % · zu · 🔋 96 %“
    var valuesText: String {
        var l: [String] = []
        if let t = temp { l.append(String(format: "%.1f °C", t).replacingOccurrences(of: ".", with: ",")) }
        if let h = hum { l.append("\(Int(h.rounded())) %") }
        if let o = open { l.append(o ? "offen" : "zu") }
        if leak == true { l.append("Wasser!") }
        for (k, v) in (vals ?? [:]).sorted(by: { $0.key < $1.key }) {
            let name = ["co2": "CO₂", "voc": "VOC", "tvoc": "TVOC", "pm25": "PM2,5", "pm2_5": "PM2,5", "pm10": "PM10", "aqi": "AQI"][k] ?? k
            l.append("\(name) \(Int(v.rounded()))")
        }
        if let b = bat, b >= 0 { l.append("🔋 \(b) %") }
        return l.joined(separator: " · ")
    }
}

struct ProtectSummary: Decodable, Hashable {
    var at: Int64?
    var err: String?
    var version: String?
    var devs: [ProtectDev]?
}

struct PWGroup: Decodable, Hashable, Identifiable {
    let id: String
    let name: String
    var note: String?
}

struct StateResponse: Decodable {
    let now: Int64
    let hosts: [HostView]
    let sites: [SiteView]
    var groups: [PWGroup]?
    let version: String
    let push: Bool?
}

struct OverviewResponse: Decodable {
    let from: Int64
    let bucket: Int64
    let n: Int
    let hosts: [String: [Agg]]
}

struct SeriesResponse: Decodable {
    let from: Int64
    let to: Int64
    let bucket: Int64
    let b: [Agg]
    let threshold: Int
    let baseline: Int
}

struct DayStat: Decodable, Identifiable {
    let day: String
    let hours: [Agg]
    let total: Agg
    let outages: Int
    let downtime: Int64
    let lossEvents: Int
    let spikeEvents: Int
    var id: String { day }
}

struct StatsResponse: Decodable {
    let days: [DayStat]
}

struct DevicePrefs: Codable {
    var token: String
    var name: String
    var sandbox: Bool
    var outage: Bool
    var loss: Bool
    var spike: Bool
    var offline: Bool
    var hosts: [String]
}

// MARK: - API-Client

enum APIError: LocalizedError {
    case unauthorized
    case http(Int, String)
    case invalidServer

    var errorDescription: String? {
        switch self {
        case .unauthorized: return "Nicht angemeldet oder Passwort geändert."
        case .http(let c, let m): return m.isEmpty ? "Serverfehler \(c)" : m
        case .invalidServer: return "Server-Adresse ungültig."
        }
    }
}

struct API {
    let creds: Credentials

    static let session: URLSession = {
        let c = URLSessionConfiguration.default
        c.timeoutIntervalForRequest = 20
        c.httpCookieStorage = nil
        c.httpShouldSetCookies = false
        return URLSession(configuration: c)
    }()

    static func normalize(_ input: String) -> URL? {
        var s = input.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.isEmpty { return nil }
        if !s.lowercased().hasPrefix("http") { s = "https://" + s }
        while s.hasSuffix("/") { s.removeLast() }
        return URL(string: s)
    }

    /// Meldet sich mit E-Mail und Passwort an (E-Mail leer = Betreiber-Zugang) und liefert das Zugangs-Token.
    static func login(server: URL, email: String = "", password: String) async throws -> Credentials {
        var req = URLRequest(url: server.appendingPathComponent("api/app/login"))
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(["email": email.trimmingCharacters(in: .whitespaces).lowercased(), "password": password])
        let (data, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw APIError.invalidServer }
        if http.statusCode == 401 { throw APIError.http(401, "E-Mail oder Passwort falsch.") }
        guard http.statusCode == 200 else {
            throw APIError.http(http.statusCode, String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "")
        }
        struct R: Decodable { let token: String }
        guard let r = try? JSONDecoder().decode(R.self, from: data) else {
            throw APIError.http(http.statusCode, "Das ist kein PingWacht-Server.")
        }
        return Credentials(server: server, token: r.token)
    }

    private func request(_ path: String, query: [String: String] = [:], method: String = "GET", body: Data? = nil) async throws -> Data {
        var comps = URLComponents(url: creds.server.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        if !query.isEmpty { comps.queryItems = query.map { URLQueryItem(name: $0.key, value: $0.value) } }
        var req = URLRequest(url: comps.url!)
        req.httpMethod = method
        req.setValue("Bearer \(creds.token)", forHTTPHeaderField: "Authorization")
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, resp) = try await Self.session.data(for: req)
        guard let http = resp as? HTTPURLResponse else { throw APIError.invalidServer }
        if http.statusCode == 401 { throw APIError.unauthorized }
        guard (200..<300).contains(http.statusCode) else {
            throw APIError.http(http.statusCode, String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "")
        }
        return data
    }

    private func get<T: Decodable>(_ path: String, _ query: [String: String] = [:]) async throws -> T {
        try JSONDecoder().decode(T.self, from: try await request(path, query: query))
    }

    func state() async throws -> StateResponse { try await get("api/state") }

    func overview(hours: Int = 24, n: Int = 48) async throws -> OverviewResponse {
        try await get("api/overview", ["hours": "\(hours)", "n": "\(n)"])
    }

    func series(host: String, from: Date, to: Date, n: Int = 300) async throws -> SeriesResponse {
        try await get("api/series", ["host": host, "from": "\(from.ms)", "to": "\(to.ms)", "n": "\(n)"])
    }

    func events(host: String? = nil, types: [String] = [], from: Date? = nil, to: Date? = nil, limit: Int = 300) async throws -> [PWEvent] {
        var q = ["limit": "\(limit)"]
        if let host { q["host"] = host }
        if !types.isEmpty { q["types"] = types.joined(separator: ",") }
        if let from { q["from"] = "\(from.ms)" }
        if let to { q["to"] = "\(to.ms)" }
        return try await get("api/events", q)
    }

    /// Ereignis quittieren („ich kümmere mich“) – stoppt die Eskalation.
    func ack(event id: Int64, note: String = "") async throws {
        struct Body: Encodable { let id: Int64; let note: String }
        _ = try await request("api/events/ack", method: "POST", body: try JSONEncoder().encode(Body(id: id, note: note)))
    }

    /// Netzkarte als Liste: Orte → Sektoren → Kunden.
    func mapTree(id: String? = nil, hideOld: Bool = false) async throws -> MapTree {
        var q: [String: String] = [:]
        if let id { q["id"] = id }
        if hideOld { q["old"] = "0" }
        return try await get("api/maps/tree", q)
    }

    func stats(host: String, days: Int) async throws -> StatsResponse {
        try await get("api/stats", ["host": host, "days": "\(days)"])
    }

    func registerDevice(_ prefs: DevicePrefs) async throws {
        _ = try await request("api/app/device", method: "POST", body: try JSONEncoder().encode(prefs))
    }

    func unregisterDevice(token: String) async throws {
        _ = try await request("api/app/device", query: ["id": token], method: "DELETE")
    }

    func pushTest() async throws -> String {
        let d = try await request("api/app/pushtest", method: "POST")
        struct R: Decodable { let sent: Int; let errors: [String]? }
        let r = try JSONDecoder().decode(R.self, from: d)
        if let e = r.errors, !e.isEmpty { return "Fehler: " + e.joined(separator: ", ") }
        return "Testnachricht an \(r.sent) Gerät(e) gesendet."
    }
}

// MARK: - Hilfsfunktionen

extension Date {
    init(ms: Int64) { self.init(timeIntervalSince1970: TimeInterval(ms) / 1000) }
    var ms: Int64 { Int64(timeIntervalSince1970 * 1000) }
}

/// Formatiert Zehntel-ms als Text, z. B. 123 → "12,3 ms"
func fmtMs(_ tenths: Int) -> String {
    guard tenths >= 0 else { return "–" }
    return fmtMs(Double(tenths) / 10)
}

func fmtMs(_ ms: Double?) -> String {
    guard let ms else { return "–" }
    if ms < 1 { return "<1 ms" }
    if ms < 10 { return String(format: "%.1f ms", ms).replacingOccurrences(of: ".", with: ",") }
    return "\(Int(ms.rounded())) ms"
}

func fmtPct(_ ratio: Double) -> String {
    if ratio == 0 { return "0 %" }
    let p = ratio * 100
    if p < 1 { return String(format: "%.2f %%", p).replacingOccurrences(of: ".", with: ",") }
    return String(format: "%.1f %%", p).replacingOccurrences(of: ".", with: ",")
}

func fmtDuration(_ seconds: TimeInterval) -> String {
    let s = Int(seconds.rounded())
    if s < 60 { return "\(s) s" }
    let m = s / 60
    if m < 60 { return m < 10 && s % 60 != 0 ? "\(m) min \(s % 60) s" : "\(m) min" }
    let h = m / 60
    if h < 24 { return m % 60 != 0 ? "\(h) h \(m % 60) min" : "\(h) h" }
    return "\(h / 24) T \(h % 24) h"
}

private let timeFmt: DateFormatter = { let f = DateFormatter(); f.locale = Locale(identifier: "de_DE"); f.dateFormat = "HH:mm"; return f }()
private let timeSecFmt: DateFormatter = { let f = DateFormatter(); f.locale = Locale(identifier: "de_DE"); f.dateFormat = "HH:mm:ss"; return f }()
private let dayFmt: DateFormatter = { let f = DateFormatter(); f.locale = Locale(identifier: "de_DE"); f.dateFormat = "EE dd.MM."; return f }()

func fmtTime(_ d: Date, seconds: Bool = false) -> String {
    (seconds ? timeSecFmt : timeFmt).string(from: d)
}

func fmtDay(_ d: Date) -> String {
    let cal = Calendar.current
    if cal.isDateInToday(d) { return "Heute" }
    if cal.isDateInYesterday(d) { return "Gestern" }
    return dayFmt.string(from: d)
}

func fmtDayTime(_ d: Date, seconds: Bool = false) -> String {
    fmtDay(d) + " " + fmtTime(d, seconds: seconds)
}


// MARK: - Netzkarte (Liste)

struct MapTreeNode: Decodable, Hashable, Identifiable {
    let id: String
    let name: String
    var addr: String?
    let st: String          // up, down, warn, paused, unknown
    var rtt: Int?
    var host: String?
    var total: Int?
    var down: Int?
    var kids: [MapTreeNode]?

    var isDown: Bool { st == "down" }
    var rttText: String? { rtt.map { String(format: "%.1f ms", Double($0) / 10) } }
}

struct MapTreeZone: Decodable, Hashable, Identifiable {
    var id: String { name }
    let name: String
    let total: Int
    let down: Int
    let sectors: [MapTreeNode]
}

struct MapTreeRef: Decodable, Hashable, Identifiable {
    let id: String
    let name: String
}

struct MapTree: Decodable {
    var map: MapTreeRef?
    var maps: [MapTreeRef]?
    var at: Int64?
    var checked: Bool?
    let zones: [MapTreeZone]
}
