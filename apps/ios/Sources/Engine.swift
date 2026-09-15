import Foundation
import Network

/// LinkKind is an uplink the phone can send on.
enum LinkKind: String, CaseIterable, Identifiable {
    case wifi
    case cellular

    var id: String { rawValue }

    var interface: NWInterface.InterfaceType {
        switch self {
        case .wifi: return .wifi
        case .cellular: return .cellular
        }
    }

    var label: String {
        switch self {
        case .wifi: return "Wi-Fi"
        case .cellular: return "Mobile data"
        }
    }

    /// Cellular costs money per byte. The UI colours it warm for that reason.
    var metered: Bool { self == .cellular }
}

/// Probe result for a URL.
struct Probed {
    let host: String
    let path: String
    let size: Int64
    let ranges: Bool
    let filename: String
}

enum EngineError: LocalizedError {
    case badURL
    case noSize
    case tooSmall
    case noLinks(String)

    var errorDescription: String? {
        switch self {
        case .badURL: return "That does not look like an http or https link."
        case .noSize: return "The server would not say how big the file is."
        case .tooSmall: return "That file is empty."
        case .noLinks(let why): return why
        }
    }
}

/// Live progress for the UI.
struct EngineProgress {
    var owners: [String] = []      // chunk index -> link that fetched it
    var bytes: Int64 = 0
    var total: Int64 = 0
    var elapsed: TimeInterval = 0
    var perLink: [String: Int64] = [:]
    var finished = false
    var note: String?

    var mbps: Double { elapsed > 0 ? Double(bytes) * 8 / elapsed / 1e6 : 0 }
}

/// Engine downloads one file, splitting it by byte range across every link that
/// works, mirroring the Go implementation: one shared queue of chunks, several
/// workers per link, so a slow link simply completes fewer chunks instead of
/// holding up a fixed share.
actor Engine {
    private let chunkSize: Int64
    private let workersPerLink: Int

    init(chunkSize: Int64 = 2 << 20, workersPerLink: Int = 4) {
        self.chunkSize = chunkSize
        self.workersPerLink = workersPerLink
    }

    /// probe asks whether a URL can be split, and how big it is. A one-byte
    /// ranged GET is used rather than a HEAD because CDNs misreport HEAD while a
    /// 206 is proof.
    func probe(_ urlText: String, over kinds: [LinkKind]) async throws -> Probed {
        guard let url = URL(string: urlText),
              let host = url.host,
              url.scheme == "http" || url.scheme == "https"
        else { throw EngineError.badURL }

        let path = url.path.isEmpty ? "/" : url.path + (url.query.map { "?\($0)" } ?? "")
        var lastError: Error?

        for kind in kinds {
            let conn = PinnedConnection(interface: kind.interface, host: host,
                                        port: url.scheme == "https" ? 443 : 80,
                                        tls: url.scheme == "https")
            do {
                let reply = try await conn.get(path: path, range: 0 ... 0)
                await conn.close()

                var size: Int64 = 0
                var ranges = false
                if reply.status == 206, let cr = reply.header("content-range"),
                   let total = cr.split(separator: "/").last, let n = Int64(total) {
                    size = n
                    ranges = true
                } else if reply.status == 200, let len = reply.header("content-length"), let n = Int64(len) {
                    size = n
                } else if reply.status >= 400 {
                    throw HTTPError.badStatus(reply.status)
                } else {
                    throw EngineError.noSize
                }
                guard size > 0 else { throw EngineError.tooSmall }

                let name = url.lastPathComponent.isEmpty ? "download" : url.lastPathComponent
                return Probed(host: host, path: path, size: size, ranges: ranges, filename: name)
            } catch {
                lastError = error
                await conn.close()
            }
        }
        throw EngineError.noLinks("Could not reach that link: \(lastError?.localizedDescription ?? "unknown")")
    }

    /// download fetches the whole file to `destination`, reporting progress.
    /// Only links that actually carry bytes on their own interface are counted,
    /// so a link that silently falls back cannot be credited.
    func download(_ probed: Probed, urlScheme: String, over kinds: [LinkKind],
                  to destination: URL,
                  onProgress: @escaping @Sendable (EngineProgress) -> Void) async throws -> EngineProgress {
        // A server that ignores ranges cannot be split, so one link does the
        // work and the UI says so rather than implying a bond.
        let useRanges = probed.ranges
        let effectiveKinds = useRanges ? kinds : [kinds.first ?? .wifi]

        // Two forces pull against each other. Work stealing needs many more
        // chunks than workers, or the split freezes at the start and the whole
        // transfer waits for the slowest link. But every chunk costs a round
        // trip, because a connection does one request at a time, so small
        // chunks bleed throughput: 256 KB chunks measured 50.4 Mbps where
        // 512 KB measured 59.6 on the same link.
        //
        // So keep chunks large and add workers instead, and only bond at all
        // when the file is big enough to pay for the extra connections.
        let minBondable: Int64 = 8 << 20
        let worthBonding = useRanges && probed.size >= minBondable && kinds.count > 1
        let effectiveKinds2 = worthBonding ? effectiveKinds : [effectiveKinds.first ?? .wifi]

        let effectiveChunk = max(1 << 20, min(chunkSize, probed.size / 16))
        let chunks = max(1, Int((probed.size + effectiveChunk - 1) / effectiveChunk))
        // Four chunks per worker leaves enough in the queue for a fast link to
        // take more than its share without starving the pool.
        let perLink = max(1, min(workersPerLink, chunks / (effectiveKinds2.count * 4)))

        FileManager.default.createFile(at: destination, size: probed.size)
        let handle = try FileHandle(forWritingTo: destination)
        defer { try? handle.close() }

        let state = DownloadState(chunks: chunks, total: probed.size)
        let started = Date()

        await withTaskGroup(of: Void.self) { group in
            for kind in effectiveKinds2 {
                for _ in 0 ..< (useRanges ? perLink : 1) {
                    group.addTask { [effectiveChunk] in
                        let conn = PinnedConnection(
                            interface: kind.interface, host: probed.host,
                            port: urlScheme == "https" ? 443 : 80,
                            tls: urlScheme == "https"
                        )
                        defer { Task { await conn.close() } }

                        while let index = await state.lease() {
                            let start = Int64(index) * effectiveChunk
                            let end = min(start + effectiveChunk - 1, probed.size - 1)
                            do {
                                let reply = try await conn.get(path: probed.path,
                                                               range: useRanges ? start ... end : nil)
                                guard useRanges ? reply.status == 206 : reply.status == 200 else {
                                    throw HTTPError.badStatus(reply.status)
                                }
                                let want = Int(end - start + 1)
                                guard useRanges ? reply.body.count == want : !reply.body.isEmpty else {
                                    throw HTTPError.shortBody(got: reply.body.count, want: want)
                                }
                                // The interface is confirmed per chunk, so a
                                // silent fallback is attributed honestly rather
                                // than credited to the link we asked for.
                                let actual = await conn.actualInterface()
                                await state.complete(index: index, owner: actual,
                                                     bytes: Int64(reply.body.count), start: start,
                                                     body: reply.body, handle: handle)
                                let snap = await state.snapshot(started: started)
                                onProgress(snap)
                            } catch {
                                await state.fail(index: index, error: error)
                            }
                        }
                    }
                }
            }
        }

        var final = await state.snapshot(started: started)
        final.finished = true
        if !useRanges {
            final.note = "This server does not support byte ranges, so it could not be split. One link was used."
        } else if !worthBonding && kinds.count > 1 {
            final.note = "Too small to be worth splitting — under 8 MB the extra connections cost more than they save."
        }
        if let failure = await state.firstFailure, await state.remaining > 0 {
            throw failure
        }
        onProgress(final)
        return final
    }
}

/// DownloadState is the shared queue plus the bitmap, guarded by actor
/// isolation rather than a lock.
actor DownloadState {
    private var pending: [Int]
    private var done: Set<Int> = []
    private let total: Int64
    private var attempts: [Int: Int] = [:]
    private var bytes: Int64 = 0
    private var owners: [String]
    private var perLink: [String: Int64] = [:]
    private(set) var firstFailure: Error?

    private let maxAttempts = 4

    init(chunks: Int, total: Int64) {
        pending = Array(0 ..< chunks)
        owners = Array(repeating: "", count: chunks)
        self.total = total
    }

    var remaining: Int { owners.count - done.count }

    func lease() -> Int? {
        if firstFailure != nil { return nil }
        while let next = pending.first {
            pending.removeFirst()
            if done.contains(next) { continue }
            return next
        }
        return nil
    }

    func complete(index: Int, owner: String, bytes n: Int64, start: Int64, body: Data, handle: FileHandle) {
        guard !done.contains(index) else { return }
        // Bytes reach the file before the chunk is marked complete, the same
        // ordering the Go engine needs so a reader never sees a hole it has
        // been told is filled.
        try? handle.seek(toOffset: UInt64(start))
        try? handle.write(contentsOf: body)
        done.insert(index)
        owners[index] = owner
        bytes += n
        perLink[owner, default: 0] += n
    }

    func fail(index: Int, error: Error) {
        attempts[index, default: 0] += 1
        if attempts[index]! >= maxAttempts {
            if firstFailure == nil { firstFailure = error }
            return
        }
        pending.append(index)
    }

    func snapshot(started: Date) -> EngineProgress {
        EngineProgress(
            owners: owners,
            bytes: bytes,
            total: total,
            elapsed: Date().timeIntervalSince(started),
            perLink: perLink,
            finished: false,
            note: nil
        )
    }
}

extension FileManager {
    /// createFile makes a sparse file of the right size so every chunk can be
    /// written at its true offset with no merge pass afterwards.
    func createFile(at url: URL, size: Int64) {
        try? removeItem(at: url)
        createFile(atPath: url.path, contents: nil)
        if let h = try? FileHandle(forWritingTo: url) {
            try? h.truncate(atOffset: UInt64(size))
            try? h.close()
        }
    }
}
