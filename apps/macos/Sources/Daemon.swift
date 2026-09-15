import Foundation

/// Daemon owns the bundled Go engine.
///
/// The engine is embedded rather than reimplemented: it carries the tested
/// scheduler, the resume logic, and the handling for servers that lie about
/// byte ranges. The app drives it over the loopback.
@MainActor
final class Daemon: ObservableObject {
    @Published private(set) var running = false
    @Published private(set) var failure: String?

    private(set) var port: Int = 0
    private(set) var token: String = ""

    private var process: Process?
    /// Distinguishes a deliberate shutdown from a crash, so only the crash is
    /// worth recovering from.
    private var stopping = false
    private var restarts = 0

    /// The address is known as soon as a port is chosen. It must NOT depend on
    /// `running`: the readiness check builds its URL from here, so requiring
    /// `running` first made it impossible to ever become ready — every button
    /// stayed disabled and the event stream never connected.
    var baseURL: URL? {
        guard port > 0 else { return nil }
        return URL(string: "http://127.0.0.1:\(port)")
    }

    /// authed appends the engine's token, which every endpoint requires.
    func authed(_ path: String, query: [String: String] = [:]) -> URL? {
        guard let base = baseURL, var comps = URLComponents(url: base.appendingPathComponent(path),
                                                            resolvingAgainstBaseURL: false) else { return nil }
        var items = query.map { URLQueryItem(name: $0.key, value: $0.value) }
        items.append(URLQueryItem(name: "t", value: token))
        comps.queryItems = items
        return comps.url
    }

    func start() {
        guard process == nil else { return }
        stopping = false

        guard let exec = Bundle.main.url(forResource: "braid-engine", withExtension: nil) else {
            failure = "The download engine is missing from the app bundle."
            return
        }

        token = UUID().uuidString
        port = Daemon.freePort()

        let cache = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("braid", isDirectory: true)
        try? FileManager.default.createDirectory(at: cache, withIntermediateDirectories: true)

        let p = Process()
        p.executableURL = exec
        // The engine exits on its own if this app is force-quit, which a
        // termination handler would never get to do. Without it, engines pile
        // up holding ports and still downloading.
        p.arguments = [
            "serve", "-port", String(port), "-token", token, "-cache", cache.path,
            "-watch-parent", String(ProcessInfo.processInfo.processIdentifier),
        ]
        p.standardOutput = Pipe()
        p.standardError = Pipe()
        p.terminationHandler = { [weak self] proc in
            Task { @MainActor in
                guard let self else { return }
                self.running = false
                self.process = nil
                guard !self.stopping else { return }

                // The engine died on its own. Sitting there dead with a window
                // full of zeroes is the worst outcome, so bring it back — but
                // bounded, or a genuinely broken engine becomes a restart loop.
                if self.restarts < 5 {
                    self.restarts += 1
                    self.failure = "Restarting the download engine…"
                    try? await Task.sleep(nanoseconds: 600_000_000)
                    self.start()
                } else {
                    self.failure = "The download engine keeps stopping (code \(proc.terminationStatus))."
                }
            }
        }

        do {
            try p.run()
            process = p
            // The engine binds its port before printing anything, so a short
            // poll is more reliable than parsing its output.
            Task { await waitUntilReady() }
        } catch {
            failure = "Could not start the download engine: \(error.localizedDescription)"
        }
    }

    private func waitUntilReady() async {
        guard let probe = authed("api/links") else { return }
        for _ in 0 ..< 40 {
            if let (_, resp) = try? await URLSession.shared.data(from: probe),
               (resp as? HTTPURLResponse)?.statusCode == 200 {
                running = true
                failure = nil
                restarts = 0
                return
            }
            try? await Task.sleep(nanoseconds: 150_000_000)
        }
        failure = "The download engine did not come up."
    }

    func stop() {
        stopping = true
        process?.terminate()
        process = nil
        running = false
    }

    /// freePort asks the kernel for an unused port rather than guessing one and
    /// colliding with whatever else is running.
    private static func freePort() -> Int {
        let sock = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(sock) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        addr.sin_port = 0
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(sock, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bound == 0 else { return 18422 }
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        var out = sockaddr_in()
        let got = withUnsafeMutablePointer(to: &out) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                getsockname(sock, $0, &len)
            }
        }
        guard got == 0 else { return 18422 }
        return Int(UInt16(bigEndian: out.sin_port))
    }
}
