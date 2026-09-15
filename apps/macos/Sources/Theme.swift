import SwiftUI

/// Cool hues are free, warm hues cost money. The rule is the whole palette:
/// a glance at the weave tells you how much of a transfer is being billed to
/// you, without reading a number.
enum Theme {
    static let free: [Color] = [
        Color(red: 0.17, green: 0.65, blue: 0.77),
        Color(red: 0.49, green: 0.56, blue: 0.88),
        Color(red: 0.25, green: 0.66, blue: 0.54),
    ]
    static let cost: [Color] = [
        Color(red: 0.88, green: 0.58, blue: 0.18),
        Color(red: 0.78, green: 0.40, blue: 0.30),
    ]

    static let ink = Color(red: 0.90, green: 0.93, blue: 0.94)
    static let inkSoft = Color(red: 0.62, green: 0.71, blue: 0.74)
    static let inkFaint = Color(red: 0.37, green: 0.48, blue: 0.53)
    static let ground = Color(red: 0.039, green: 0.133, blue: 0.161)
    static let surface = Color(red: 0.059, green: 0.180, blue: 0.216)
    static let rule = Color(red: 0.11, green: 0.255, blue: 0.298)
    static let pending = Color(red: 0.09, green: 0.208, blue: 0.243)

    /// Colours are assigned per interface and kept, so a link looks the same in
    /// the weave, the legend and the ledger.
    static func colour(for iface: String, links: [LinkInfo]) -> Color {
        guard let link = links.first(where: { $0.iface == iface }) else {
            return iface == "resumed" ? inkFaint : inkFaint
        }
        let pool = link.metered ? cost : free
        let peers = links.filter { $0.metered == link.metered }
        let index = peers.firstIndex(where: { $0.iface == iface }) ?? 0
        return pool[index % pool.count]
    }
}

func prettyBytes(_ n: Int64) -> String {
    if n < 1024 { return "\(n) B" }
    if n < 1_048_576 { return String(format: "%.1f KB", Double(n) / 1024) }
    if n < 1_073_741_824 { return String(format: "%.1f MB", Double(n) / 1_048_576) }
    return String(format: "%.2f GB", Double(n) / 1_073_741_824)
}

func prettySeconds(_ s: Double) -> String {
    if !s.isFinite || s < 0 { return "—" }
    if s < 60 { return "\(Int(s))s" }
    let m = Int(s) / 60
    return "\(m)m\(String(format: "%02d", Int(s) % 60))s"
}

/// Weave draws the file from byte zero to the end, each slice painted in the
/// colour of the link that fetched it. Canvas rather than a stack of shapes:
/// a large file has hundreds of chunks and SwiftUI views would crawl.
struct Weave: View {
    let owners: [String]
    let links: [LinkInfo]

    var body: some View {
        Canvas { context, size in
            guard !owners.isEmpty else {
                context.fill(Path(CGRect(origin: .zero, size: size)), with: .color(Theme.pending))
                return
            }
            let width = size.width / CGFloat(owners.count)
            for (i, owner) in owners.enumerated() {
                let rect = CGRect(x: CGFloat(i) * width, y: 0,
                                  width: width.rounded(.up) + 0.5, height: size.height)
                let colour: Color = owner.isEmpty
                    ? Theme.pending
                    : (owner == "resumed" ? Theme.inkFaint : Theme.colour(for: owner, links: links))
                context.fill(Path(rect), with: .color(colour))
            }
        }
        .clipShape(RoundedRectangle(cornerRadius: 5, style: .continuous))
        .animation(.easeOut(duration: 0.25), value: owners)
    }
}

/// A quiet card that lets the weave and the headline number carry the page.
struct Panel<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        content
            .padding(18)
            .background(Theme.surface, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
            .overlay(
                RoundedRectangle(cornerRadius: 10, style: .continuous)
                    .strokeBorder(Theme.rule, lineWidth: 1)
            )
    }
}
