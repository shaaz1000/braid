import AVKit
import SwiftUI

@main
struct BraidMacApp: App {
    @StateObject private var daemon = Daemon()
    @StateObject private var client = Client()

    var body: some Scene {
        WindowGroup {
            DashboardView()
                .environmentObject(daemon)
                .environmentObject(client)
                .frame(minWidth: 620, minHeight: 620)
                .onAppear {
                    daemon.start()
                    client.connect(to: daemon)
                }
                .onDisappear { daemon.stop() }
        }
        .windowStyle(.hiddenTitleBar)
        .defaultSize(width: 720, height: 780)

        // Live speed in the menu bar, because "is my cellular being spent right
        // now" is a glance question, not a window question.
        MenuBarExtra {
            MenuBarPanel()
                .environmentObject(daemon)
                .environmentObject(client)
        } label: {
            MenuBarLabel().environmentObject(client)
        }
        .menuBarExtraStyle(.window)
    }
}

struct MenuBarLabel: View {
    @EnvironmentObject var client: Client

    var body: some View {
        let active = client.transfers.last(where: { !$0.finished })
        HStack(spacing: 5) {
            Image(systemName: "point.3.filled.connected.trianglepath.dotted")
            if let active {
                Text(String(format: "%.0f", active.mbps)).monospacedDigit()
            }
        }
    }
}

struct MenuBarPanel: View {
    @EnvironmentObject var client: Client

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let t = client.transfers.last {
                Text(t.name).font(.headline).lineLimit(1)
                Weave(owners: t.owners, links: client.links).frame(height: 26)
                HStack {
                    Text(String(format: "%.1f Mbps", t.mbps)).monospacedDigit()
                    Spacer()
                    Text("\(prettyBytes(t.bytes)) of \(prettyBytes(t.size))")
                        .foregroundStyle(.secondary)
                }
                .font(.callout)
            } else {
                Text("Nothing downloading").foregroundStyle(.secondary)
            }

            Divider()
            ForEach(client.links) { link in
                HStack(spacing: 7) {
                    Circle()
                        .fill(Theme.colour(for: link.iface, links: client.links))
                        .frame(width: 7, height: 7)
                    Text(link.label).font(.callout)
                    if link.metered {
                        Text("billed").font(.caption).foregroundStyle(.orange)
                    }
                }
            }
            Divider()
            Button("Quit braid") { NSApplication.shared.terminate(nil) }
        }
        .padding(14)
        .frame(width: 280)
    }
}

struct DashboardView: View {
    @EnvironmentObject var daemon: Daemon
    @EnvironmentObject var client: Client

    @State private var urlText = ""
    @State private var note: String?
    @State private var playing: URL?

    private var latest: TransferInfo? { client.transfers.last }

    var body: some View {
        ZStack {
            Theme.ground.ignoresSafeArea()
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    header
                    hero
                    ledger
                    composer
                    if let note {
                        Text(note).font(.callout).foregroundStyle(Theme.inkSoft)
                    }
                    Spacer(minLength: 0)
                }
                .padding(26)
            }
        }
        .foregroundStyle(Theme.ink)
        .sheet(item: $playing) { url in
            VideoPlayer(player: AVPlayer(url: url))
                .frame(minWidth: 760, minHeight: 460)
        }
        // Dropping a link is faster than pasting one.
        .onDrop(of: [.url, .text], isTargeted: nil) { providers in
            guard let provider = providers.first else { return false }
            _ = provider.loadObject(ofClass: URL.self) { url, _ in
                if let url { Task { @MainActor in urlText = url.absoluteString } }
            }
            return true
        }
    }

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            Text("braid").font(.system(size: 26, weight: .semibold)).tracking(-0.5)
            Spacer()
            HStack(spacing: 8) {
                ForEach(client.links) { link in
                    Circle()
                        .fill(Theme.colour(for: link.iface, links: client.links))
                        .frame(width: 9, height: 9)
                        .help(link.label + (link.metered ? " — billed" : ""))
                }
                Text(client.links.isEmpty
                     ? (daemon.running ? "no uplinks" : "starting…")
                     : "\(client.links.count) uplink\(client.links.count == 1 ? "" : "s")")
                    .font(.callout).foregroundStyle(Theme.inkSoft)
            }
        }
    }

    /// The hero is the transfer itself: two streams of light entering, braiding,
    /// leaving as one. Particle density per lane is that link's real share and
    /// the flow speed is the measured rate, so it races when the download does.
    private var hero: some View {
        VStack(alignment: .leading, spacing: 16) {
            Flow(links: client.links,
                 shares: liveShares(),
                 mbps: latest?.mbps ?? 0,
                 active: latest.map { !$0.finished } ?? false)
                .frame(height: 168)

            HStack(alignment: .lastTextBaseline, spacing: 10) {
                if let t = latest, t.cached {
                    Text(prettyBytes(t.size))
                        .font(.system(size: 54, weight: .medium)).tracking(-2)
                    Text("already here").font(.system(size: 18, weight: .medium))
                        .foregroundStyle(Theme.inkSoft)
                } else {
                    Odometer(value: latest?.mbps ?? 0)
                        .pulse(on: latest?.finished ?? false)
                    Text("Mbps").font(.system(size: 18, weight: .medium))
                        .foregroundStyle(Theme.inkSoft)
                }
                Spacer()
                Sparkline(history: client.history,
                          colour: Theme.free[0])
                    .frame(width: 168, height: 44)
            }

            Text(latest.map(subtitle) ?? (client.links.count > 1
                 ? "Paste or drop a link and braid pulls it over every uplink at once."
                 : "Only one uplink. Tether a phone over USB, or plug in Ethernet, to bond."))
                .font(.callout).foregroundStyle(Theme.inkSoft)

            if let t = latest, !t.owners.isEmpty {
                Weave(owners: t.owners, links: client.links)
                    .frame(height: 30)
            }
        }
    }

    /// liveShares is each link's fraction of the work so far, which drives how
    /// dense that lane of light is.
    private func liveShares() -> [String: Double] {
        guard let t = latest, !t.owners.isEmpty else { return [:] }
        var counts: [String: Int] = [:]
        for o in t.owners where !o.isEmpty { counts[o, default: 0] += 1 }
        let total = counts.values.reduce(0, +)
        guard total > 0 else { return [:] }
        return counts.mapValues { Double($0) / Double(total) }
    }

    private func subtitle(for t: TransferInfo) -> String {
        if let failed = t.failed, !failed.isEmpty { return failed }
        if t.cached { return "\(t.name) — already on disk, nothing was downloaded again." }
        let progress = "\(prettyBytes(t.bytes)) of \(prettyBytes(t.size))"
        let time = t.finished ? "finished in \(prettySeconds(t.seconds))" : "\(prettySeconds(t.seconds)) elapsed"
        return "\(t.name) — \(progress) · \(time)"
    }

    private var ledger: some View {
        VStack(spacing: 0) {
            ForEach(shares(), id: \.iface) { share in
                VStack(alignment: .leading, spacing: 7) {
                    HStack {
                        Text(share.label).font(.body.weight(.medium))
                        if share.metered {
                            Text("\(prettyBytes(share.bytes)) billed")
                                .font(.caption.weight(.medium))
                                .foregroundStyle(Theme.cost[0])
                        }
                        Spacer()
                        Text("\(Int(share.fraction * 100))%  ·  \(prettyBytes(share.bytes))")
                            .font(.callout).monospacedDigit().foregroundStyle(Theme.inkSoft)
                    }
                    GeometryReader { geo in
                        ZStack(alignment: .leading) {
                            Capsule().fill(Theme.pending)
                            Capsule()
                                .fill(Theme.colour(for: share.iface, links: client.links))
                                .frame(width: max(2, geo.size.width * share.fraction))
                                .animation(.easeOut(duration: 0.35), value: share.fraction)
                        }
                    }
                    .frame(height: 6)
                }
                .padding(.vertical, 13)
                Divider().overlay(Theme.rule)
            }
        }
    }

    private struct Share {
        let iface: String
        let label: String
        let metered: Bool
        let fraction: Double
        let bytes: Int64
    }

    private func shares() -> [Share] {
        guard let t = latest, !t.owners.isEmpty else { return [] }
        var counts: [String: Int] = [:]
        for owner in t.owners where !owner.isEmpty { counts[owner, default: 0] += 1 }
        let total = counts.values.reduce(0, +)
        guard total > 0 else { return [] }

        return counts.map { iface, n in
            let fraction = Double(n) / Double(total)
            let link = client.links.first { $0.iface == iface }
            return Share(
                iface: iface,
                label: link?.label ?? (iface == "resumed" ? "Already on disk" : iface),
                metered: link?.metered ?? false,
                fraction: fraction,
                bytes: Int64(fraction * Double(t.bytes))
            )
        }
        .sorted { $0.fraction > $1.fraction }
    }

    private var composer: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 9) {
                TextField("Paste a link", text: $urlText)
                    .textFieldStyle(.plain)
                    .font(.body)
                    .padding(11)
                    .background(Theme.surface, in: RoundedRectangle(cornerRadius: 7))
                    .overlay(RoundedRectangle(cornerRadius: 7).strokeBorder(Theme.rule))
                    .onSubmit { begin() }

                Button("Download") { begin() }
                    .buttonStyle(.borderedProminent)
                    .disabled(urlText.isEmpty || !daemon.running)
            }

            HStack(spacing: 9) {
                Button("Play here") {
                    guard let url = client.streamURL(for: urlText, on: daemon) else { return }
                    playing = url
                }
                .disabled(urlText.isEmpty || !daemon.running)

                Button("Copy link for other devices") {
                    guard let url = client.streamURL(for: urlText, on: daemon) else { return }
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(url.absoluteString, forType: .string)
                    note = "Copied. Paste it into VLC, Infuse, or a browser on this network."
                }
                .disabled(urlText.isEmpty || !daemon.running)

                Spacer()
                if let failure = daemon.failure {
                    Text(failure).font(.callout).foregroundStyle(Theme.cost[1])
                }
            }
            .buttonStyle(.bordered)
        }
    }

    private func begin() {
        let text = urlText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }
        note = nil
        Task {
            do {
                try await client.start(text, on: daemon)
            } catch {
                note = error.localizedDescription
            }
        }
    }
}

extension URL: Identifiable {
    public var id: String { absoluteString }
}
