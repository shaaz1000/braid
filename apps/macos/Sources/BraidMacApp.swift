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
                .frame(minWidth: 680, minHeight: 640)
                .onAppear {
                    daemon.start()
                    client.connect(to: daemon)
                }
                .onDisappear { daemon.stop() }
        }
        .windowStyle(.hiddenTitleBar)
        .defaultSize(width: 760, height: 700)

        // Live speed in the menu bar, because "is my cellular being spent right
        // now" is a glance question, not a window question.
        MenuBarExtra {
            MenuBarPanel().environmentObject(client)
        } label: {
            MenuBarLabel().environmentObject(client)
        }
        .menuBarExtraStyle(.window)
    }
}

struct MenuBarLabel: View {
    @EnvironmentObject var client: Client

    var body: some View {
        HStack(spacing: 5) {
            Image(systemName: "point.3.filled.connected.trianglepath.dotted")
            if let active = client.transfers.last(where: { !$0.finished }) {
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
                Weave(owners: t.owners, links: client.links).frame(height: 24)
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
    @State private var dropping = false

    private var latest: TransferInfo? { client.transfers.last }
    private var bonded: Bool { client.links.count > 1 }
    private var busy: Bool { latest.map { !$0.finished } ?? false }

    var body: some View {
        ZStack {
            Theme.ground.ignoresSafeArea()
            VStack(alignment: .leading, spacing: 18) {
                header
                Flow(links: client.links, shares: shares(), mbps: latest?.mbps ?? 0, active: busy)
                    .frame(height: 176)
                rateRow
                linkCards
                if let t = latest, !t.owners.isEmpty {
                    Weave(owners: t.owners, links: client.links).frame(height: 26)
                }
                Spacer(minLength: 0)
                composer
            }
            .padding(24)
        }
        .foregroundStyle(Theme.ink)
        .overlay(alignment: .top) { if dropping { dropHint } }
        .sheet(item: $playing) { url in
            VideoPlayer(player: AVPlayer(url: url)).frame(minWidth: 780, minHeight: 470)
        }
        .onDrop(of: [.url, .text], isTargeted: $dropping) { providers in
            guard let provider = providers.first else { return false }
            _ = provider.loadObject(ofClass: URL.self) { url, _ in
                if let url { Task { @MainActor in urlText = url.absoluteString; begin() } }
            }
            return true
        }
    }

    // MARK: - header

    private var header: some View {
        HStack(alignment: .firstTextBaseline) {
            Text("braid").font(.system(size: 25, weight: .semibold)).tracking(-0.6)
            Text(statusLine)
                .font(.system(size: 13))
                .foregroundStyle(bonded ? Theme.free[0] : Theme.inkSoft)
            Spacer()
            if let t = latest, t.finished, !t.cached {
                Text("done in \(prettySeconds(t.seconds))")
                    .font(.system(size: 13)).foregroundStyle(Theme.inkSoft)
            }
        }
    }

    private var statusLine: String {
        if !daemon.running && client.links.isEmpty { return "starting the engine…" }
        switch client.links.count {
        case 0: return "no uplinks found"
        case 1: return "one uplink — nothing to bond yet"
        default: return "\(client.links.count) uplinks bonded"
        }
    }

    // MARK: - rate

    private var rateRow: some View {
        HStack(alignment: .lastTextBaseline, spacing: 12) {
            if let t = latest, t.cached {
                Text(prettyBytes(t.size)).font(.system(size: 52, weight: .semibold)).tracking(-1.5)
                Text("already here").font(.system(size: 17, weight: .medium)).foregroundStyle(Theme.inkSoft)
            } else {
                Odometer(value: latest?.mbps ?? 0).pulse(on: latest?.finished ?? false)
                Text("Mbps").font(.system(size: 17, weight: .medium)).foregroundStyle(Theme.inkSoft)
            }
            Spacer()
            Sparkline(history: client.history, colour: Theme.free[0])
                .frame(width: 190, height: 42)
        }
        .overlay(alignment: .bottomLeading) {
            Text(caption).font(.system(size: 13)).foregroundStyle(Theme.inkSoft)
                .offset(y: 22).lineLimit(1)
        }
        .padding(.bottom, 20)
    }

    private var caption: String {
        guard let t = latest else {
            return bonded
                ? "Paste or drop a link. braid pulls it over every uplink at once."
                : "Tether a phone over USB, or plug in Ethernet, and braid will use both."
        }
        if let failed = t.failed, !failed.isEmpty { return failed }
        if t.cached { return "\(t.name) — already on disk, nothing downloaded again." }
        return "\(t.name) — \(prettyBytes(t.bytes)) of \(prettyBytes(t.size))"
    }

    // MARK: - links

    private var linkCards: some View {
        HStack(spacing: 12) {
            if client.links.isEmpty {
                Panel {
                    Text(daemon.running ? "No uplinks found." : "Starting the engine…")
                        .font(.system(size: 13)).foregroundStyle(Theme.inkSoft)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            } else {
                ForEach(client.links) { link in
                    LinkCard(
                        link: link,
                        share: shares()[link.iface] ?? 0,
                        bytes: Int64((shares()[link.iface] ?? 0) * Double(latest?.bytes ?? 0)),
                        colour: Theme.colour(for: link.iface, links: client.links),
                        idle: (shares()[link.iface] ?? 0) == 0
                    )
                }
                if client.links.count == 1 {
                    // An empty slot that says what to do, rather than a blank gap.
                    VStack(alignment: .leading, spacing: 8) {
                        Text("Add a second uplink").font(.system(size: 14, weight: .medium))
                        Text("Tether a phone over USB, or plug in Ethernet. braid picks it up on its own.")
                            .font(.system(size: 12)).foregroundStyle(Theme.inkSoft)
                            .fixedSize(horizontal: false, vertical: true)
                        Spacer()
                    }
                    .padding(14)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(
                        RoundedRectangle(cornerRadius: 10, style: .continuous)
                            .strokeBorder(style: StrokeStyle(lineWidth: 1, dash: [5, 4]))
                            .foregroundStyle(Theme.rule)
                    )
                }
            }
        }
        .frame(height: 124)
    }

    private func shares() -> [String: Double] {
        guard let t = latest, !t.owners.isEmpty else { return [:] }
        var counts: [String: Int] = [:]
        for o in t.owners where !o.isEmpty { counts[o, default: 0] += 1 }
        let total = counts.values.reduce(0, +)
        guard total > 0 else { return [:] }
        return counts.mapValues { Double($0) / Double(total) }
    }

    // MARK: - composer

    private var composer: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 9) {
                TextField("Paste a link, or drop one anywhere", text: $urlText)
                    .textFieldStyle(.plain)
                    .font(.system(size: 14))
                    .padding(11)
                    .background(Theme.surface, in: RoundedRectangle(cornerRadius: 8))
                    .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Theme.rule))
                    .onSubmit(begin)

                Button("Download", action: begin)
                    .buttonStyle(.borderedProminent)
                    .disabled(urlText.isEmpty || !daemon.running)
                Button("Play", action: play)
                    .disabled(urlText.isEmpty || !daemon.running)
                Button("Copy link", action: copyLink)
                    .disabled(urlText.isEmpty || !daemon.running)
            }
            if let message = note ?? daemon.failure {
                Text(message).font(.system(size: 12)).foregroundStyle(Theme.inkSoft).lineLimit(2)
            }
        }
    }

    private var dropHint: some View {
        Text("Drop to download over every uplink")
            .font(.system(size: 13, weight: .medium))
            .padding(.horizontal, 16).padding(.vertical, 10)
            .background(Theme.free[0].opacity(0.9), in: Capsule())
            .foregroundStyle(Theme.ground)
            .padding(.top, 14)
    }

    // MARK: - actions

    private func begin() {
        let text = urlText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }
        note = nil
        Task {
            do { try await client.start(text, on: daemon) }
            catch { note = error.localizedDescription }
        }
    }

    private func play() {
        guard let url = client.streamURL(for: urlText, on: daemon) else { return }
        playing = url
    }

    private func copyLink() {
        guard let url = client.streamURL(for: urlText, on: daemon) else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(url.absoluteString, forType: .string)
        note = "Copied. Paste it into VLC, Infuse, or a browser on this network."
    }
}

extension URL: Identifiable {
    public var id: String { absoluteString }
}
