import Foundation

// MARK: - Konto, Rechner, Stummschaltungen

struct MeInfo: Decodable, Hashable {
    var name: String?
    var email: String?
    var role: String?
    var `super`: Bool?
    var canEdit: Bool { role != "viewer" }
}

struct OrgInfo: Decodable, Hashable {
    let id: String
    let name: String
    var plan: String?
    var features: [String]?
    var modules: [String]?
}

struct OrgRef: Decodable, Hashable, Identifiable {
    let id: String
    let name: String
}

struct MuteView: Decodable, Hashable, Identifiable {
    let id: String
    let name: String
    let end: Int64
    var forever: Bool?
    var all: Bool?
    var hosts: Int?
}

/// Rechner (RMM) – Zustand laut Agent
struct MachineView: Decodable, Hashable, Identifiable {
    let id: String
    let site: String
    let name: String
    var host: String?
    var group: String?
    var desc: String?
    var online: Bool?
    var status: String?
    var problems: [String]?
    var cpu: Int?
    var mem: Int?
    var disk: Int?
    var diskName: String?
    var svcOK: Int?
    var svcN: Int?
    var rebootSince: Int64?
    var at: Int64?

    var statusText: String {
        switch status {
        case "ok": return "OK"
        case "warn": return "Warnung"
        case "bad": return "Problem"
        case "off": return "offline"
        case "disabled": return "Standort offline"
        default: return "unbekannt"
        }
    }
}

// MARK: - UniFi-Details (Ports, WLAN, Clients)

struct UXPort: Decodable, Hashable, Identifiable {
    let idx: Int
    var name: String?
    let up: Bool
    var speed: Int?
    var max: Int?
    var conn: String?
    var poe: String?
    var poeOn: Bool?
    var poeState: String?
    var poeW: Double?
    var uplink: Bool?
    var id: Int { idx }
    var speedText: String {
        guard up, let s = speed, s > 0 else { return "–" }
        return s >= 1000 ? "\(s / 1000) Gbit/s" : "\(s) Mbit/s"
    }
}

struct UXRadio: Decodable, Hashable {
    let band: String
    var std: String?
    var channel: Int?
    var width: Int?
    var util: Double?
    var clients: Int?
    var retries: Double?
}

struct UXClient: Decodable, Hashable, Identifiable {
    let name: String
    var ip: String?
    var mac: String?
    var type: String?
    var since: Int64?
    var uplink: String?
    var id: String { (mac ?? "") + name + (ip ?? "") }
    var typeText: String { type == "WIRELESS" ? "WLAN" : type == "WIRED" ? "Kabel" : (type ?? "") }
}

struct UXDetail: Decodable {
    let name: String
    var model: String?
    var fw: String?
    var upgrade: String?
    var uplink: String?
    var ports: [UXPort]?
    var radios: [UXRadio]?
    var clients: [UXClient]?
    var canReboot: Bool?
    var warn: [String]?
}

// MARK: - Fritzbox, MikroTik, Netonix (Kurzansicht)

struct FritzStatusInfo: Decodable, Hashable {
    var model: String?
    var fw: String?
    var boxUptime: Int64?
    var access: String?
    var link: String?
    var conn: String?
    var connUptime: Int64?
    var extIP: String?
    var down: Int64?
    var up: Int64?
    var dsl: Bool?
}

struct FritzVoIP: Decodable, Hashable { let number: String; var name: String?; var status: String?; var registrar: String? }
struct FritzCall: Decodable, Hashable { let type: String; let date: String; var other: String?; var name: String?; var own: String?; var device: String?; var dur: String? }
struct FritzDefl: Decodable, Hashable { let on: Bool; var number: String?; var to: String?; var mode: String? }
struct FritzWLAN: Decodable, Hashable { let idx: Int; let ssid: String; let on: Bool; var channel: Int?; var band: String?; let clients: Int }
struct FritzDect: Decodable, Hashable { var id: String?; let name: String; var model: String?; let active: Bool; var update: Bool? }

struct FritzExtraInfo: Decodable, Hashable {
    var wlan: [FritzWLAN]?
    var voip: [FritzVoIP]?
    var calls: [FritzCall]?
    var defl: [FritzDefl]?
    var dect: [FritzDect]?
    var updAvail: Bool?
    var updVersion: String?
    var missing: [String]?
}

struct FritzInfo: Decodable {
    let on: Bool
    var at: Int64?
    var err: String?
    var status: FritzStatusInfo?
    var drops24: Int?
    var boots24: Int?
    var avail: Double?
    var extra: FritzExtraInfo?
}

struct MtKV: Decodable, Hashable { let n: String; let v: String }
struct MtSnapInfo: Decodable {
    var res: [String: String]?
    var upd: [String: String]?
    var health: [MtKV]?
    var ppp: Int?
    var dhcp: Int?
    var conns: Int64?
    var miss: [String]?
}
struct MtInfo: Decodable {
    let on: Bool
    var at: Int64?
    var err: String?
    var cur: MtSnapInfo?
    var boots24: Int?
    var open: [String]?
    var cpuMax24: Double?
}

struct NxPortInfo: Decodable, Hashable, Identifiable {
    let n: Int
    var name: String?
    let link: String
    let poe: String
    let w: Double
    let en: Bool
    var id: Int { n }
}
struct NxSnapInfo: Decodable {
    var name: String?
    var model: String?
    var fw: String?
    var up: Int64?
    var cpu: Double?
    var cpuT: Double?
    var boardT: Double?
    var v48: Double?
    var ports: [NxPortInfo]?
}
struct NxInfo: Decodable {
    let on: Bool
    var at: Int64?
    var err: String?
    var cur: NxSnapInfo?
    var boots24: Int?
    var open: [String]?
}

// MARK: - Aufrufe

extension API {
    /// Host/Standort/Gruppe pausieren („pause“) oder nur Meldungen stummschalten („mute“). until in ms, -1 = dauerhaft.
    func mute(scope: String, id: String, mode: String, until: Int64) async throws {
        struct B: Encodable { let scope: String; let id: String; let mode: String; let until: Int64 }
        _ = try await request("api/mute", method: "POST", body: try JSONEncoder().encode(B(scope: scope, id: id, mode: mode, until: until)))
    }

    /// Pause aufheben.
    func resume(scope: String, id: String) async throws {
        try await mute(scope: scope, id: id, mode: "resume", until: -1)
    }

    /// Stummschaltung beenden.
    func endMute(id: String) async throws {
        _ = try await request("api/maint", query: ["id": id], method: "DELETE")
    }

    func unifiDevice(site: String, id: String, usite: String, mac: String) async throws -> UXDetail {
        try await get("api/unifi/device", ["site": site, "id": id, "usite": usite, "mac": mac])
    }

    func unifiClients(site: String) async throws -> [UXClient] {
        try await get("api/unifi/clients", ["site": site])
    }

    /// Neustart („restart“) oder PoE-Neustart eines Ports („poe“).
    func unifiAction(site: String, usite: String, id: String, name: String, action: String, port: Int = 0) async throws -> String {
        struct B: Encodable { let site, usite, id, name, action: String; let port: Int }
        let d = try await request("api/unifi/devaction", method: "POST", body: try JSONEncoder().encode(B(site: site, usite: usite, id: id, name: name, action: action, port: port)))
        struct R: Decodable { let msg: String? }
        return (try? JSONDecoder().decode(R.self, from: d))?.msg ?? "Ausgelöst"
    }

    func snapshot(site: String, camera: String) async throws -> Data {
        try await request("api/unifi/snapshot", query: ["site": site, "cam": camera, "t": "\(Date().ms)"])
    }

    func fritz(host: String) async throws -> FritzInfo { try await get("api/fritz", ["host": host]) }
    func mikrotik(host: String) async throws -> MtInfo { try await get("api/mikrotik", ["host": host, "hours": "1"]) }
    func netonix(host: String) async throws -> NxInfo { try await get("api/netonix", ["host": host, "hours": "1"]) }

    /// Rechner neu starten (nur ohne Signaturpflicht des Kontos möglich).
    func rebootMachine(id: String, code: String = "") async throws {
        struct B: Encodable { let kind, scope, id, reboot, code: String }
        _ = try await request("api/actions", method: "POST", body: try JSONEncoder().encode(B(kind: "reboot", scope: "machine", id: id, reboot: "now", code: code)))
    }
}

func fmtBitrate(_ bps: Int64?) -> String {
    guard let b = bps, b > 0 else { return "–" }
    let k = Double(b) / 1000
    if k >= 1_000_000 { return String(format: "%.2f Gbit/s", k / 1_000_000).replacingOccurrences(of: ".", with: ",") }
    if k >= 1000 { return String(format: "%.1f Mbit/s", k / 1000).replacingOccurrences(of: ".", with: ",") }
    return "\(Int(k)) kbit/s"
}
