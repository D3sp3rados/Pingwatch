import SwiftUI

enum PW {
    static let accent = Color(red: 0.20, green: 0.83, blue: 0.60)
    static let ok = Color.green
    static let warn = Color.orange
    static let loss = Color(red: 0.98, green: 0.45, blue: 0.09)
    static let bad = Color.red
    static let off = Color.gray

    static func color(forStatus s: String) -> Color {
        switch s {
        case "ok": return ok
        case "spike": return warn
        case "loss": return loss
        case "down": return bad
        default: return off
        }
    }

    static func color(for a: Agg) -> Color {
        if a.n == 0 { return Color.gray.opacity(0.25) }
        if a.lossRatio >= 0.5 { return bad }
        if a.lost > 0 { return loss }
        if a.spk > 0 { return warn }
        return ok
    }

    static func color(forEvent type: String) -> Color {
        switch type {
        case "outage", "device", "port": return bad
        case "loss": return loss
        case "spike": return warn
        default: return off
        }
    }
}

struct StatusDot: View {
    let color: Color
    var size: CGFloat = 10
    var body: some View {
        Circle().fill(color).frame(width: size, height: size)
    }
}

/// 24-h-Balken: jede Säule ein Zeitabschnitt, Farbe nach Zustand.
struct TimelineBar: View {
    let buckets: [Agg]
    var height: CGFloat = 18

    var body: some View {
        GeometryReader { geo in
            HStack(spacing: buckets.count > 60 ? 0.5 : 1) {
                ForEach(Array(buckets.enumerated()), id: \.offset) { _, a in
                    Rectangle().fill(PW.color(for: a))
                }
            }
            .frame(width: geo.size.width, height: height)
            .clipShape(RoundedRectangle(cornerRadius: 4))
        }
        .frame(height: height)
    }
}
