import Foundation

struct LinkInfo: Decodable, Identifiable {
    let iface: String
    let label: String
    let metered: Bool
    var id: String { iface }
}

struct TransferInfo: Decodable, Identifiable {
    let id: String
    let name: String
    let url: String
    let size: Int64
    let chunks: Int
    let done: Int
    let bytes: Int64
    let owners: [String]
    let mbps: Double
    let seconds: Double
    let finished: Bool
    let cached: Bool
    let failed: String?

    var fraction: Double { chunks > 0 ? Double(done) / Double(chunks) : 0 }
}

struct Snapshot: Decodable {
    let links: [LinkInfo]
    let transfers: [TransferInfo]
}

/// Client follows the engine's server-sent event stream.
///
/// Polling would either lag the weave or hammer the engine; the engine already
/// pushes a snapshot four times a second, so the app just reads it.
@MainActor
final class Client: ObservableObject {
    @Published var links: [LinkInfo] = []
    @Published var transfers: [TransferInfo] = []
    @Published var connected = false
    /// A rolling window of throughput, so the sparkline can show a link
    /// dropping out or a transfer ramping up rather than only a current value.
    @Published var history: [Double] = []

    private var task: Task<Void, Never>?

    /// A dedicated session. URLSession.shared times a request out after 60s,
    /// which would cut an event stream that is meant to stay open, and
    /// `.infinity` is rejected outright rather than meaning "no timeout" —
    /// which is why the window sat on "starting…" while the engine was
    /// happily streaming.
    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = 3600
        config.timeoutIntervalForResource = 86_400
        config.waitsForConnectivity = true
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        return URLSession(configuration: config)
    }()

    func connect(to daemon: Daemon) {
        task?.cancel()
        task = Task { [weak self] in
            while !Task.isCancelled {
                await self?.follow(daemon)
                // The engine may still be starting, or may have been restarted.
                try? await Task.sleep(nanoseconds: 700_000_000)
            }
        }
    }

    func disconnect() {
        task?.cancel()
        task = nil
        connected = false
    }

    private func follow(_ daemon: Daemon) async {
        guard let url = daemon.authed("api/events") else { return }
        // Seed from a plain request first, so the window shows the real links
        // immediately even if the stream is slow to establish.
        await refreshOnce(daemon)

        let request = URLRequest(url: url)
        do {
            let (bytes, response) = try await session.bytes(for: request)
            guard (response as? HTTPURLResponse)?.statusCode == 200 else { return }
            connected = true
            for try await line in bytes.lines {
                guard line.hasPrefix("data: ") else { continue }
                let json = String(line.dropFirst(6))
                guard let data = json.data(using: .utf8),
                      let snap = try? JSONDecoder().decode(Snapshot.self, from: data) else { continue }
                links = snap.links
                transfers = snap.transfers

                let live = snap.transfers.last(where: { !$0.finished })?.mbps ?? 0
                history.append(live)
                if history.count > 120 { history.removeFirst(history.count - 120) }
            }
        } catch {
            // A dropped stream is normal when the engine restarts; the caller
            // reconnects rather than surfacing it as an error.
        }
        connected = false
    }

    /// refreshOnce fetches a single snapshot. The stream is the main path, but
    /// a one-shot read means an empty window can never be the first thing you
    /// see while the stream is still connecting.
    private func refreshOnce(_ daemon: Daemon) async {
        guard let url = daemon.authed("api/links") else { return }
        struct LinksOnly: Decodable { let links: [LinkInfo] }
        guard let (data, _) = try? await session.data(from: url),
              let decoded = try? JSONDecoder().decode(LinksOnly.self, from: data) else { return }
        links = decoded.links
    }

    /// start asks the engine to fetch a URL. The engine serves the bytes as it
    /// gets them, so this returns as soon as the transfer is under way.
    func start(_ urlText: String, on daemon: Daemon) async throws {
        guard let stream = daemon.authed("stream", query: ["url": urlText]) else { return }
        var request = URLRequest(url: stream)
        request.httpMethod = "GET"
        // Ask for one byte: enough to make the engine begin, without pulling the
        // whole file through the app. The engine keeps fetching in the
        // background and serves the rest from disk.
        request.setValue("bytes=0-0", forHTTPHeaderField: "Range")
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "braid", code: http.statusCode, userInfo: [
                NSLocalizedDescriptionKey: "The engine could not fetch that link (HTTP \(http.statusCode))."
            ])
        }
    }

    /// streamURL is what you hand to a player or another device.
    func streamURL(for urlText: String, on daemon: Daemon) -> URL? {
        daemon.authed("stream", query: ["url": urlText])
    }
}
