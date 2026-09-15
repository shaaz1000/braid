import Network
import SwiftUI

@main
struct BraidSpikeApp: App {
    var body: some Scene {
        WindowGroup {
            SpikeView()
        }
    }
}

/// What the spike is trying to find out, in one sentence each.
private let questions = [
    "Can an app pin one connection to Wi-Fi and another to cellular?",
    "Does the connection actually use the interface it was told to?",
    "Do both together beat the faster one alone?",
]

@MainActor
final class SpikeModel: ObservableObject {
    @Published var results: [LinkResult] = []
    @Published var combined: [LinkResult] = []
    @Published var running = false
    @Published var status = "Ready."
    @Published var verdict: String?
    @Published var verdictGood = false

    /// A real file on a CDN that honours byte ranges, which is what braid needs.
    private let host = "dl.google.com"
    private let path = "/go/go1.27.1.darwin-arm64.tar.gz"
    /// Bounded so a run cannot quietly eat a large amount of mobile data.
    private let rangeEnd = 20_000_000
    private let seconds: TimeInterval = 6

    func run() async {
        running = true
        results = []
        combined = []
        verdict = nil
        defer { running = false }

        // Each link alone, so there is a baseline to compare the pair against.
        for kind in [NWInterface.InterfaceType.wifi, .cellular] {
            status = "Measuring \(Puller.name(kind)) alone…"
            let p = Puller(interface: kind, host: host, path: path, rangeEnd: rangeEnd)
            let r = await p.run(for: seconds)
            results.append(r)
        }

        // Both at once. This is the number the whole idea rests on.
        status = "Measuring both together…"
        async let wifi = Puller(interface: .wifi, host: host, path: path, rangeEnd: rangeEnd).run(for: seconds)
        async let cell = Puller(interface: .cellular, host: host, path: path, rangeEnd: rangeEnd).run(for: seconds)
        combined = await [wifi, cell]

        judge()
        status = "Done."
    }

    private func judge() {
        let cellular = results.first { $0.intended == "cellular" }

        // Pinning is the premise. If asking for cellular hands back Wi-Fi, every
        // throughput number below is measuring one link twice.
        guard let cellular, cellular.error == nil, cellular.bytes > 0 else {
            verdictGood = false
            verdict = "Cellular could not be used at all\(cellular?.error.map { ": \($0)" } ?? "."). "
                + "Without it there is nothing to bond on this phone."
            return
        }
        if !cellular.pinned {
            verdictGood = false
            verdict = "Pinning failed: asked for cellular, the connection used \(cellular.actual). "
                + "iOS is not honouring requiredInterfaceType here, so on-phone bonding is not possible this way."
            return
        }

        let bestAlone = results.map(\.mbps).max() ?? 0
        let together = combined.reduce(0) { $0 + $1.mbps }
        let gain = bestAlone > 0 ? (together / bestAlone - 1) * 100 : 0

        if together > bestAlone * 1.15 {
            verdictGood = true
            verdict = String(format: "Bonding works on the phone: %.1f Mbps together against %.1f alone, %+.0f%%.",
                             together, bestAlone, gain)
        } else {
            verdictGood = false
            verdict = String(format: "Both links are pinned, but together gave %.1f Mbps against %.1f alone. "
                             + "No useful gain, so this is not worth building on.", together, bestAlone)
        }
    }
}

struct SpikeView: View {
    @StateObject private var model = SpikeModel()

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 22) {
                    Text("Before building a bonded downloader on the phone, three things have to be true.")
                        .foregroundStyle(.secondary)

                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(questions, id: \.self) { q in
                            HStack(alignment: .firstTextBaseline, spacing: 8) {
                                Circle().frame(width: 5, height: 5).foregroundStyle(.tertiary)
                                Text(q)
                            }
                            .font(.callout)
                        }
                    }

                    Button {
                        Task { await model.run() }
                    } label: {
                        Text(model.running ? "Measuring…" : "Measure")
                            .frame(maxWidth: .infinity)
                            .padding(.vertical, 6)
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(model.running)

                    Text(model.status).font(.footnote).foregroundStyle(.secondary)

                    if let verdict = model.verdict {
                        Text(verdict)
                            .font(.callout.weight(.medium))
                            .padding(14)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .background(model.verdictGood ? Color.green.opacity(0.14) : Color.orange.opacity(0.16))
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                    }

                    if !model.results.isEmpty {
                        section("Each link alone", model.results)
                    }
                    if !model.combined.isEmpty {
                        section("Both at once", model.combined)
                        let total = model.combined.reduce(0) { $0 + $1.mbps }
                        Text(String(format: "%.1f Mbps combined", total))
                            .font(.system(size: 34, weight: .medium))
                            .monospacedDigit()
                    }

                    Text("Each run transfers up to about 20 MB per link. Roughly a third of that is mobile data.")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
                .padding(20)
            }
            .navigationTitle("braid spike")
        }
    }

    private func section(_ title: String, _ rows: [LinkResult]) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(title).font(.headline)
            ForEach(rows) { r in
                VStack(alignment: .leading, spacing: 3) {
                    HStack {
                        Text(r.intended).font(.body.weight(.medium))
                        if !r.pinned {
                            Text("used \(r.actual)")
                                .font(.caption.weight(.medium))
                                .padding(.horizontal, 6).padding(.vertical, 2)
                                .background(Color.orange.opacity(0.2))
                                .clipShape(Capsule())
                        }
                        Spacer()
                        Text(String(format: "%.1f Mbps", r.mbps)).monospacedDigit()
                    }
                    Text("\(r.bytes / 1_048_576) MB in \(String(format: "%.1f", r.seconds))s")
                        .font(.caption).foregroundStyle(.secondary)
                    if let e = r.error {
                        Text(e).font(.caption).foregroundStyle(.orange)
                    }
                }
                .padding(.vertical, 4)
                Divider()
            }
        }
    }
}
