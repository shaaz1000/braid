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
    /// High enough that no link can finish its range inside the window. A 20 MB
    /// cap let Wi-Fi finish in 1.3s, so the "concurrent" phase was mostly
    /// cellular running alone and the summed rate meant nothing.
    private let rangeEnd = 600_000_000
    /// Several connections per link, because a single TCP stream cannot
    /// saturate a 5G radio. One stream measured cellular at 17.7 Mbps where the
    /// Mac measured 112.8 Mbps over the same phone using four.
    private let streams = 4
    private let seconds: TimeInterval = 4

    func run() async {
        running = true
        results = []
        combined = []
        verdict = nil
        defer { running = false }

        // Each link alone, so there is a baseline to compare the pair against.
        for kind in [NWInterface.InterfaceType.wifi, .cellular] {
            status = "Measuring \(Puller.name(kind)) alone…"
            let r = await measureLink(kind)
            results.append(r)
            log(r, phase: "alone")
        }

        // Both at once, over the same window. This is the number the whole idea
        // rests on, and it is only meaningful if neither link finishes early.
        status = "Measuring both together…"
        async let wifi = measureLink(.wifi)
        async let cell = measureLink(.cellular)
        combined = await [wifi, cell]
        for r in combined { log(r, phase: "together") }

        judge()
        print("SPIKE verdict: \(verdict ?? "none") [good=\(verdictGood)]")
        status = "Done."
    }

    /// measureLink runs several pinned connections at once and reports their
    /// combined throughput, which is how the real engine uses a link.
    private func measureLink(_ kind: NWInterface.InterfaceType) async -> LinkResult {
        let started = Date()
        var parts: [LinkResult] = []
        await withTaskGroup(of: LinkResult.self) { group in
            for _ in 0 ..< streams {
                group.addTask { [host, path, rangeEnd, seconds] in
                    await Puller(interface: kind, host: host, path: path, rangeEnd: rangeEnd)
                        .run(for: seconds)
                }
            }
            for await r in group { parts.append(r) }
        }

        let bytes = parts.reduce(0) { $0 + $1.bytes }
        // Wall clock, not the sum of per-stream times, or parallel streams would
        // divide the elapsed time and inflate the rate.
        let elapsed = Date().timeIntervalSince(started)
        let actual = parts.first(where: { $0.bytes > 0 })?.actual
            ?? parts.first?.actual ?? "none"
        return LinkResult(
            intended: Puller.name(kind),
            actual: actual,
            bytes: bytes,
            seconds: elapsed,
            error: bytes > 0 ? nil : parts.compactMap(\.error).first
        )
    }

    private func log(_ r: LinkResult, phase: String) {
        print(String(format: "SPIKE %@ asked=%@ actual=%@ pinned=%@ bytes=%d secs=%.2f mbps=%.1f err=%@",
                     phase, r.intended, r.actual, r.pinned ? "yes" : "NO",
                     r.bytes, r.seconds, r.mbps, r.error ?? "-"))
    }

    private func judge() {
        // Every link must have actually carried bytes on the interface it was
        // asked for. Without this, a run where Wi-Fi was off compared cellular
        // against cellular and reported a "+55% gain" that was nothing but
        // ordinary variance between two measurements of one link.
        let alone = Dictionary(uniqueKeysWithValues: results.map { ($0.intended, $0) })

        for name in ["wifi", "cellular"] {
            guard let r = alone[name] else {
                verdictGood = false
                verdict = "\(name) was never measured, so nothing can be concluded."
                return
            }
            if r.bytes == 0 {
                verdictGood = false
                let why = r.error ?? "no data transferred"
                verdict = "\(name) carried no data (\(why)). "
                    + (name == "wifi"
                       ? "Turn Wi-Fi on while leaving Cellular Data on, then run again — a phone that is tethering has Wi-Fi switched off, and with one link there is nothing to bond."
                       : "Turn Cellular Data on, then run again.")
                return
            }
            if !r.pinned {
                verdictGood = false
                verdict = "Pinning failed: asked for \(name), the connection used \(r.actual). "
                    + "iOS is not honouring requiredInterfaceType, so bonding on the phone is not possible this way."
                return
            }
        }

        // Both links must also carry bytes in the concurrent run, or the total
        // is just one link again.
        let both = Dictionary(uniqueKeysWithValues: combined.map { ($0.intended, $0) })
        let contributed = ["wifi", "cellular"].filter { (both[$0]?.bytes ?? 0) > 0 }
        guard contributed.count == 2 else {
            verdictGood = false
            let missing = ["wifi", "cellular"].filter { !contributed.contains($0) }.joined(separator: " and ")
            verdict = "Both links work alone, but \(missing) carried nothing when run together. "
                + "iOS appears to allow only one at a time."
            return
        }

        let bestAlone = results.map(\.mbps).max() ?? 0
        let together = combined.reduce(0) { $0 + $1.mbps }
        let gain = bestAlone > 0 ? (together / bestAlone - 1) * 100 : 0

        if together > bestAlone * 1.15 {
            verdictGood = true
            verdict = String(format: "Bonding works on the phone: %.1f Mbps together against %.1f alone, %+.0f%%. "
                             + "Both links carried data on their own interface.", together, bestAlone, gain)
        } else {
            verdictGood = false
            verdict = String(format: "Both links are pinned and both carried data, but together gave %.1f Mbps "
                             + "against %.1f alone. No useful gain, so this is not worth building on.",
                             together, bestAlone)
        }
    }
}

struct SpikeView: View {
    @StateObject private var model = SpikeModel()
    @State private var hasRun = false

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

                    Text("Each run opens four connections per link for four seconds. Expect to spend roughly 20–60 MB of mobile data.")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
                .padding(20)
            }
            .navigationTitle("braid spike")
            .task {
                // Runs once on launch so a console-attached launch captures the
                // whole measurement without anyone having to tap.
                if !hasRun {
                    hasRun = true
                    await model.run()
                }
            }
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
