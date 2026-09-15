import Foundation
import Network

/// One measurement of one link.
struct LinkResult: Identifiable, Sendable {
    let id = UUID()
    /// The interface we asked for.
    let intended: String
    /// The interface the connection reports it actually used. This is the
    /// ground truth: asking for cellular and silently getting Wi-Fi would look
    /// like success while proving nothing, exactly the trap that made the macOS
    /// spike verify against kernel byte counters rather than a return code.
    let actual: String
    let bytes: Int
    let seconds: Double
    let error: String?

    var mbps: Double { seconds > 0 ? Double(bytes) * 8 / seconds / 1e6 : 0 }
    var pinned: Bool { actual == intended }
}

/// Puller opens one TLS connection pinned to a single interface type and reads
/// as much of a ranged GET as it can within a deadline.
///
/// Network.framework is used rather than URLSession because URLSession cannot
/// pin a request to an interface: `allowsCellularAccess` only forbids cellular,
/// it cannot require it, and iOS prefers Wi-Fi whenever Wi-Fi is up. NWParameters
/// can require an interface type, which is the whole point of the exercise.
final class Puller: @unchecked Sendable {
    private let conn: NWConnection
    private let host: String
    private let path: String
    private let rangeEnd: Int
    private let intended: NWInterface.InterfaceType

    private let lock = NSLock()
    private var bytes = 0
    private var actual = "none"
    private var failure: String?
    private var finished = false
    private var deadline = Date.distantFuture
    private var continuation: CheckedContinuation<LinkResult, Never>?
    private var started = Date()

    init(interface: NWInterface.InterfaceType, host: String, path: String, rangeEnd: Int) {
        self.intended = interface
        self.host = host
        self.path = path
        self.rangeEnd = rangeEnd

        let params = NWParameters(tls: NWProtocolTLS.Options(), tcp: NWProtocolTCP.Options())
        params.requiredInterfaceType = interface
        // Cellular is an "expensive" path. Without this, iOS refuses to use it
        // while Wi-Fi is available, which would make the pin silently fail.
        params.prohibitExpensivePaths = false
        params.prohibitConstrainedPaths = false
        // We are doing the aggregation ourselves at the byte-range level.
        // Multipath TCP needs an entitlement and needs the server to support it,
        // and almost none do.
        params.multipathServiceType = .disabled
        params.preferNoProxies = true

        conn = NWConnection(host: NWEndpoint.Host(host), port: 443, using: params)
    }

    static func name(_ t: NWInterface.InterfaceType) -> String {
        switch t {
        case .wifi: return "wifi"
        case .cellular: return "cellular"
        case .wiredEthernet: return "ethernet"
        case .loopback: return "loopback"
        case .other: return "other"
        @unknown default: return "unknown"
        }
    }

    /// run measures for `duration` and always returns, even on failure.
    func run(for duration: TimeInterval) async -> LinkResult {
        await withCheckedContinuation { cont in
            lock.lock()
            continuation = cont
            started = Date()
            deadline = Date().addingTimeInterval(duration)
            lock.unlock()

            conn.stateUpdateHandler = { [weak self] state in
                guard let self else { return }
                switch state {
                case .ready:
                    self.noteActualInterface()
                    self.sendRequest()
                case .failed(let err):
                    self.finish(error: "connection failed: \(err.localizedDescription)")
                case .cancelled:
                    self.finish(error: nil)
                case .waiting(let err):
                    // On a pinned cellular connection this is where a refusal
                    // shows up, so it must not be swallowed.
                    self.finish(error: "waiting: \(err.localizedDescription)")
                default:
                    break
                }
            }
            conn.start(queue: DispatchQueue.global(qos: .userInitiated))

            // A hard stop, so a link that never becomes ready cannot hang the run.
            DispatchQueue.global().asyncAfter(deadline: .now() + duration + 4) { [weak self] in
                self?.finish(error: nil)
            }
        }
    }

    private func noteActualInterface() {
        guard let path = conn.currentPath else { return }
        var used = "none"
        if path.usesInterfaceType(.cellular) { used = "cellular" }
        else if path.usesInterfaceType(.wifi) { used = "wifi" }
        else if path.usesInterfaceType(.wiredEthernet) { used = "ethernet" }
        else if path.usesInterfaceType(.loopback) { used = "loopback" }
        lock.lock(); actual = used; lock.unlock()
    }

    private func sendRequest() {
        let request = """
        GET \(path) HTTP/1.1\r
        Host: \(host)\r
        Range: bytes=0-\(rangeEnd)\r
        User-Agent: braid-spike/0.1\r
        Accept-Encoding: identity\r
        Connection: close\r
        \r

        """
        conn.send(content: Data(request.utf8), completion: .contentProcessed { [weak self] err in
            guard let self else { return }
            if let err {
                self.finish(error: "send failed: \(err.localizedDescription)")
                return
            }
            self.receiveLoop()
        })
    }

    private func receiveLoop() {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, isComplete, error in
            guard let self else { return }
            if let data, !data.isEmpty {
                self.lock.lock(); self.bytes += data.count; self.lock.unlock()
            }
            if let error {
                self.finish(error: "receive failed: \(error.localizedDescription)")
                return
            }
            if isComplete {
                self.finish(error: nil)
                return
            }
            self.lock.lock()
            let expired = Date() >= self.deadline
            self.lock.unlock()
            if expired {
                self.finish(error: nil)
                return
            }
            self.receiveLoop()
        }
    }

    private func finish(error: String?) {
        lock.lock()
        if finished {
            lock.unlock()
            return
        }
        finished = true
        if failure == nil { failure = error }
        let result = LinkResult(
            intended: Puller.name(intended),
            actual: actual,
            bytes: bytes,
            seconds: Date().timeIntervalSince(started),
            error: failure
        )
        let cont = continuation
        continuation = nil
        lock.unlock()

        conn.cancel()
        cont?.resume(returning: result)
    }
}
