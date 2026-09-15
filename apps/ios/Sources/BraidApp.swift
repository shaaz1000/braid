import SwiftUI

@main
struct BraidApp: App {
    var body: some Scene {
        WindowGroup { RootView() }
    }
}

// Cool hues are free, warm hues cost money — the same rule as the desktop
// dashboard, so a glance tells you how much is being billed to you.
extension Color {
    static let braidFree = Color(red: 0.17, green: 0.65, blue: 0.77)
    static let braidCost = Color(red: 0.88, green: 0.58, blue: 0.18)
    static let braidPending = Color.secondary.opacity(0.18)

    static func forLink(_ owner: String) -> Color {
        owner == "cellular" ? .braidCost : .braidFree
    }
}

func prettyBytes(_ n: Int64) -> String {
    if n < 1024 { return "\(n) B" }
    if n < 1_048_576 { return String(format: "%.1f KB", Double(n) / 1024) }
    if n < 1_073_741_824 { return String(format: "%.1f MB", Double(n) / 1_048_576) }
    return String(format: "%.2f GB", Double(n) / 1_073_741_824)
}

@MainActor
final class AppModel: ObservableObject {
    @Published var urlText = "https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz"
    @Published var progress = EngineProgress()
    @Published var busy = false
    @Published var status = ""
    @Published var error: String?

    // Speed test results, in Mbps.
    @Published var wifiAlone: Double?
    @Published var cellAlone: Double?
    @Published var bonded: Double?
    @Published var testing = false
    @Published var testStatus = ""

    private let engine = Engine()
    /// A real file on a CDN that honours ranges. 16 MB per measurement keeps a
    /// full test around 48 MB, of which roughly a third is mobile data.
    private let testURL = "https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz"
    private let testBytes: Int64 = 16 << 20

    var gain: Double? {
        guard let bonded, let best = [wifiAlone, cellAlone].compactMap({ $0 }).max(), best > 0 else { return nil }
        return (bonded / best - 1) * 100
    }

    // MARK: - speed test

    func runSpeedTest() async {
        testing = true
        error = nil
        wifiAlone = nil; cellAlone = nil; bonded = nil
        defer { testing = false; testStatus = "" }

        do {
            testStatus = "Measuring Wi-Fi alone…"
            wifiAlone = try await measure(over: [.wifi], label: "wifi")
            testStatus = "Measuring mobile data alone…"
            cellAlone = try await measure(over: [.cellular], label: "cellular")
            testStatus = "Measuring both together…"
            bonded = try await measure(over: [.wifi, .cellular], label: "both")
            print(String(format: "TEST summary wifi=%.1f cell=%.1f both=%.1f gain=%.0f%%",
                         wifiAlone ?? 0, cellAlone ?? 0, bonded ?? 0, gain ?? 0))
        } catch {
            print("TEST error: \(error.localizedDescription)")
            self.error = error.localizedDescription
        }
    }

    /// measure times a fixed slice of a real file over the given links.
    private func measure(over kinds: [LinkKind], label: String) async throws -> Double {
        let probeStart = Date()
        let probed = try await engine.probe(testURL, over: kinds)
        let probeSecs = Date().timeIntervalSince(probeStart)
        let capped = Probed(host: probed.host, path: probed.path,
                            size: min(testBytes, probed.size),
                            ranges: probed.ranges, filename: probed.filename)
        let dest = FileManager.default.temporaryDirectory.appendingPathComponent("braid-speedtest.bin")
        let started = Date()
        let result = try await engine.download(capped, urlScheme: "https", over: kinds, to: dest) { _ in }
        try? FileManager.default.removeItem(at: dest)
        let elapsed = Date().timeIntervalSince(started)
        let mbps = elapsed > 0 ? Double(result.bytes) * 8 / elapsed / 1e6 : 0
        let chunks = result.owners.count
        let byLink = result.perLink.map { "\($0.key)=\(prettyBytes($0.value))" }.sorted().joined(separator: " ")
        print(String(format: "TEST %@ mbps=%.1f bytes=%d secs=%.2f probeSecs=%.2f chunks=%d [%@]",
                     label, mbps, result.bytes, elapsed, probeSecs, chunks, byLink))
        return mbps
    }

    // MARK: - download

    func download() async {
        busy = true
        error = nil
        progress = EngineProgress()
        defer { busy = false; status = "" }

        do {
            status = "Checking the link…"
            let probed = try await engine.probe(urlText, over: [.wifi, .cellular])
            let dest = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
                .appendingPathComponent(probed.filename)
            status = "Downloading \(probed.filename)…"
            let scheme = urlText.hasPrefix("http://") ? "http" : "https"
            _ = try await engine.download(probed, urlScheme: scheme, over: [.wifi, .cellular], to: dest) { p in
                Task { @MainActor in self.progress = p }
            }
            status = "Saved to Files › braid"
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct RootView: View {
    @StateObject private var model = AppModel()
    @State private var hasAutoRun = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 26) {
                    speedTest
                    Divider()
                    downloader
                    if let e = model.error {
                        Text(e)
                            .font(.callout)
                            .foregroundStyle(.orange)
                            .padding(12)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(Color.orange.opacity(0.12))
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                    }
                    footnote
                }
                .padding(20)
            }
            .navigationTitle("braid")
            .task {
                // Auto-runs once so the numbers can be captured over the device
                // console rather than read off a screen.
                if !hasAutoRun {
                    hasAutoRun = true
                    await model.runSpeedTest()
                }
            }
        }
    }

    // MARK: - speed test

    private var speedTest: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Is it actually faster?")
                .font(.title3.weight(.semibold))
            Text("Measures each link on its own, then both at once, against the same file.")
                .font(.callout).foregroundStyle(.secondary)

            if let gain = model.gain, let bonded = model.bonded {
                VStack(alignment: .leading, spacing: 2) {
                    Text(String(format: "%.0f", bonded))
                        .font(.system(size: 62, weight: .medium))
                        .monospacedDigit()
                    + Text(" Mbps").font(.title3).foregroundStyle(.secondary)
                    Text(String(format: "%+.0f%% over the faster link alone", gain))
                        .font(.callout.weight(.medium))
                        .foregroundStyle(gain > 5 ? .green : .orange)
                }
            }

            VStack(spacing: 8) {
                bar("Wi-Fi", model.wifiAlone, .braidFree)
                bar("Mobile data", model.cellAlone, .braidCost)
                bar("Both together", model.bonded, .primary)
            }

            Button {
                Task { await model.runSpeedTest() }
            } label: {
                Text(model.testing ? "Measuring…" : "Run speed test")
                    .frame(maxWidth: .infinity).padding(.vertical, 6)
            }
            .buttonStyle(.borderedProminent)
            .disabled(model.testing || model.busy)

            if !model.testStatus.isEmpty {
                Text(model.testStatus).font(.footnote).foregroundStyle(.secondary)
            }
        }
    }

    private func bar(_ label: String, _ value: Double?, _ colour: Color) -> some View {
        let peak = max(model.wifiAlone ?? 0, model.cellAlone ?? 0, model.bonded ?? 0, 1)
        return VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(label).font(.subheadline)
                Spacer()
                Text(value.map { String(format: "%.1f Mbps", $0) } ?? "—")
                    .font(.subheadline).monospacedDigit().foregroundStyle(.secondary)
            }
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Color.braidPending)
                    Capsule().fill(colour)
                        .frame(width: geo.size.width * CGFloat((value ?? 0) / peak))
                        .animation(.easeOut(duration: 0.4), value: value)
                }
            }
            .frame(height: 6)
        }
    }

    // MARK: - downloader

    private var downloader: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Download over both links")
                .font(.title3.weight(.semibold))

            TextField("Paste a link", text: $model.urlText, axis: .vertical)
                .textFieldStyle(.roundedBorder)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .lineLimit(1 ... 3)

            Button {
                Task { await model.download() }
            } label: {
                Text(model.busy ? "Downloading…" : "Download")
                    .frame(maxWidth: .infinity).padding(.vertical, 6)
            }
            .buttonStyle(.bordered)
            .disabled(model.busy || model.testing)

            if !model.progress.owners.isEmpty {
                weave
                HStack {
                    Text(String(format: "%.1f Mbps", model.progress.mbps))
                        .font(.title2.weight(.medium)).monospacedDigit()
                    Spacer()
                    Text("\(prettyBytes(model.progress.bytes)) of \(prettyBytes(model.progress.total))")
                        .font(.callout).foregroundStyle(.secondary)
                }
                ForEach(model.progress.perLink.sorted(by: { $0.value > $1.value }), id: \.key) { owner, n in
                    HStack {
                        Circle().fill(Color.forLink(owner)).frame(width: 8, height: 8)
                        Text(owner == "cellular" ? "Mobile data" : "Wi-Fi").font(.subheadline)
                        Spacer()
                        Text(prettyBytes(n)).font(.subheadline).monospacedDigit().foregroundStyle(.secondary)
                    }
                }
            }
            if !model.status.isEmpty {
                Text(model.status).font(.footnote).foregroundStyle(.secondary)
            }
            if let note = model.progress.note {
                Text(note).font(.footnote).foregroundStyle(.orange)
            }
        }
    }

    /// The weave: the band is the file, each slice coloured by the link that
    /// fetched it, so the split is visible without reading a number.
    private var weave: some View {
        GeometryReader { geo in
            HStack(spacing: 0) {
                ForEach(Array(model.progress.owners.enumerated()), id: \.offset) { _, owner in
                    Rectangle()
                        .fill(owner.isEmpty ? Color.braidPending : Color.forLink(owner))
                }
            }
            .frame(width: geo.size.width)
            .clipShape(RoundedRectangle(cornerRadius: 3))
        }
        .frame(height: 54)
    }

    private var footnote: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("This speeds up braid's own transfers. Safari, YouTube and other apps keep using Wi-Fi only — iOS gives each app a single route and no setting changes that.")
            Text("A full speed test moves about 48 MB, roughly a third of it mobile data.")
        }
        .font(.caption)
        .foregroundStyle(.tertiary)
    }
}
