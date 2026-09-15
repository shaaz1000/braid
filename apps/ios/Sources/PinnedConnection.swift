import Foundation
import Network

enum HTTPError: LocalizedError {
    case notReady(String)
    case badStatus(Int)
    case malformedResponse(String)
    case shortBody(got: Int, want: Int)
    case cancelled

    var errorDescription: String? {
        switch self {
        case .notReady(let why): return "connection not ready: \(why)"
        case .badStatus(let code): return "server answered HTTP \(code)"
        case .malformedResponse(let why): return "malformed response: \(why)"
        case .shortBody(let got, let want): return "got \(got) bytes, expected \(want)"
        case .cancelled: return "cancelled"
        }
    }
}

struct HTTPReply {
    let status: Int
    let headers: [String: String]
    let body: Data

    func header(_ name: String) -> String? { headers[name.lowercased()] }
}

/// PinnedConnection is a persistent HTTP/1.1 connection forced onto one network
/// interface.
///
/// Network.framework is used instead of URLSession because URLSession cannot pin
/// a request to an interface: `allowsCellularAccess` only forbids cellular, it
/// cannot require it, and iOS prefers Wi-Fi whenever Wi-Fi is up. Pinning is the
/// whole mechanism, so the HTTP layer has to be written by hand.
///
/// The connection is kept alive and reused across requests. A fresh TLS
/// handshake per chunk would cost a round trip each time, which on a
/// high-latency cellular link is a large share of a chunk's transfer time.
actor PinnedConnection {
    private let conn: NWConnection
    private let host: String
    let interface: NWInterface.InterfaceType

    private var buffer = Data()
    private var opened = false

    init(interface: NWInterface.InterfaceType, host: String, port: UInt16 = 443, tls: Bool = true) {
        self.interface = interface
        self.host = host

        let params = tls
            ? NWParameters(tls: NWProtocolTLS.Options(), tcp: NWProtocolTCP.Options())
            : NWParameters(tls: nil, tcp: NWProtocolTCP.Options())
        params.requiredInterfaceType = interface
        // Cellular counts as an expensive path. Without this iOS declines to use
        // it at all while Wi-Fi is available, and the pin silently fails.
        params.prohibitExpensivePaths = false
        params.prohibitConstrainedPaths = false
        params.multipathServiceType = .disabled
        params.preferNoProxies = true

        conn = NWConnection(host: NWEndpoint.Host(host), port: .init(rawValue: port)!, using: params)
    }

    /// actualInterface is the interface the connection reports it is really
    /// using. Asking for cellular and silently being given Wi-Fi would look like
    /// success while measuring one link twice, so callers check this.
    func actualInterface() -> String {
        guard let path = conn.currentPath else { return "none" }
        if path.usesInterfaceType(.cellular) { return "cellular" }
        if path.usesInterfaceType(.wifi) { return "wifi" }
        if path.usesInterfaceType(.wiredEthernet) { return "ethernet" }
        return "none"
    }

    func open() async throws {
        if opened { return }
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            var settled = false
            conn.stateUpdateHandler = { state in
                guard !settled else { return }
                switch state {
                case .ready:
                    settled = true
                    cont.resume()
                case .failed(let err):
                    settled = true
                    cont.resume(throwing: HTTPError.notReady(err.localizedDescription))
                case .waiting(let err):
                    // A refused cellular pin surfaces here, so it must not be
                    // swallowed as a transient state.
                    settled = true
                    cont.resume(throwing: HTTPError.notReady(err.localizedDescription))
                case .cancelled:
                    settled = true
                    cont.resume(throwing: HTTPError.cancelled)
                default:
                    break
                }
            }
            conn.start(queue: .global(qos: .userInitiated))
        }
        opened = true
    }

    func close() {
        conn.cancel()
        opened = false
    }

    /// get issues one request and reads exactly the body the server declares.
    /// `range` is inclusive, matching HTTP.
    func get(path: String, range: ClosedRange<Int64>?) async throws -> HTTPReply {
        try await open()

        var lines = [
            "GET \(path) HTTP/1.1",
            "Host: \(host)",
            "User-Agent: braid-ios/0.1",
            // Identity encoding, so Content-Length describes the bytes we will
            // actually receive rather than a compressed representation.
            "Accept-Encoding: identity",
            "Connection: keep-alive",
        ]
        if let range {
            lines.append("Range: bytes=\(range.lowerBound)-\(range.upperBound)")
        }
        let request = lines.joined(separator: "\r\n") + "\r\n\r\n"
        try await send(Data(request.utf8))

        let (status, headers) = try await readHead()
        var body = Data()
        if let lengthText = headers["content-length"], let length = Int(lengthText) {
            body = try await readExactly(length)
        } else {
            // No length means the server will close to signal the end, which
            // also ends reuse of this connection.
            body = try await readUntilClose()
            opened = false
        }
        return HTTPReply(status: status, headers: headers, body: body)
    }

    // MARK: - wire

    private func send(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            conn.send(content: data, completion: .contentProcessed { err in
                if let err { cont.resume(throwing: err) } else { cont.resume() }
            })
        }
    }

    private func receiveSome() async throws -> Data {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Data, Error>) in
            conn.receive(minimumIncompleteLength: 1, maximumLength: 128 * 1024) { data, _, isComplete, error in
                if let error {
                    cont.resume(throwing: error)
                    return
                }
                if let data, !data.isEmpty {
                    cont.resume(returning: data)
                    return
                }
                if isComplete {
                    cont.resume(returning: Data())
                    return
                }
                cont.resume(returning: Data())
            }
        }
    }

    /// readHead consumes bytes until the end of the header block, leaving any
    /// body bytes that arrived in the same packet in the buffer.
    private func readHead() async throws -> (Int, [String: String]) {
        let terminator = Data("\r\n\r\n".utf8)
        while true {
            if let end = buffer.range(of: terminator) {
                let headData = buffer[buffer.startIndex ..< end.lowerBound]
                buffer.removeSubrange(buffer.startIndex ..< end.upperBound)
                return try parseHead(headData)
            }
            let more = try await receiveSome()
            if more.isEmpty {
                throw HTTPError.malformedResponse("connection closed before headers finished")
            }
            buffer.append(more)
            if buffer.count > 64 * 1024 {
                throw HTTPError.malformedResponse("header block exceeded 64 KB")
            }
        }
    }

    private func parseHead(_ data: Data) throws -> (Int, [String: String]) {
        guard let text = String(data: data, encoding: .utf8) else {
            throw HTTPError.malformedResponse("headers are not valid UTF-8")
        }
        var lines = text.components(separatedBy: "\r\n")
        guard let statusLine = lines.first else {
            throw HTTPError.malformedResponse("no status line")
        }
        let parts = statusLine.split(separator: " ", maxSplits: 2).map(String.init)
        guard parts.count >= 2, let status = Int(parts[1]) else {
            throw HTTPError.malformedResponse("unparsable status line \(statusLine)")
        }
        lines.removeFirst()

        var headers: [String: String] = [:]
        for line in lines where !line.isEmpty {
            guard let colon = line.firstIndex(of: ":") else { continue }
            let name = line[line.startIndex ..< colon].trimmingCharacters(in: .whitespaces).lowercased()
            let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
            headers[name] = value
        }
        return (status, headers)
    }

    private func readExactly(_ count: Int) async throws -> Data {
        while buffer.count < count {
            let more = try await receiveSome()
            if more.isEmpty {
                throw HTTPError.shortBody(got: buffer.count, want: count)
            }
            buffer.append(more)
        }
        let body = buffer.prefix(count)
        buffer.removeSubrange(buffer.startIndex ..< buffer.index(buffer.startIndex, offsetBy: count))
        return Data(body)
    }

    private func readUntilClose() async throws -> Data {
        while true {
            let more = try await receiveSome()
            if more.isEmpty { break }
            buffer.append(more)
        }
        let body = buffer
        buffer = Data()
        return body
    }
}
