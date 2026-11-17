import BerthSocket
import Foundation

/// Why a call to berthd failed. Every case's text is safe to show once passed through shown.
public enum ClientError: Error, Equatable, CustomStringConvertible {
    /// The process answering on the socket is not the signed berthd; nothing was sent.
    case untrustedDaemon(String)
    case unreachable(String)
    /// berthd refused the call: a Connect code such as permission_denied, its message, and the
    /// berth.v1.ErrorReason number when the error carried one.
    case refused(code: String, message: String, reason: Int?)
    case http(Int)
    case malformed(String)
    case protocolMismatch(UInt32)

    public var description: String {
        switch self {
        case .untrustedDaemon(let why): "the server on the socket is not the signed berthd (\(why))"
        case .unreachable(let why): "berthd is not reachable: \(why)"
        case .refused(let code, let message, _): message.isEmpty ? code : "\(code): \(message)"
        case .http(let status): "berthd answered HTTP \(status)"
        case .malformed(let what): "malformed answer from berthd: \(what)"
        case .protocolMismatch(let version): "berthd speaks protocol \(version), this app speaks \(protocolVersion)"
        }
    }
}

/// api.ProtocolVersion in Go: it changes only when the wire contract breaks.
public let protocolVersion: UInt32 = 1

/// One connection to berthd. read returns what has arrived and an empty array once the peer
/// closed; cancel may be called from any thread and makes a blocked read return.
public protocol Connection: AnyObject, Sendable {
    func write(_ bytes: [UInt8]) throws
    func read() throws -> [UInt8]
    func cancel()
}

/// Opens connections; the real one checks who answers before handing the connection over.
public protocol Dialer: Sendable {
    /// timeout bounds each read; nil waits forever, as a stream must between messages.
    func dial(timeout: TimeInterval?) throws -> any Connection
}

/// Dials berthd's Unix socket and checks by the peer's audit token that the process answering
/// satisfies requirement, before anything is written.
public struct SocketDialer: Dialer {
    public var path: String
    public var requirement: String

    public init(path: String, requirement: String = berthDaemonRequirement) {
        self.path = path
        self.requirement = requirement
    }

    public func dial(timeout: TimeInterval?) throws -> any Connection {
        let socket: UnixSocket
        let token: audit_token_t
        do {
            socket = try UnixSocket(path: path, timeout: timeout ?? 0)
            token = try socket.peerAuditToken()
        } catch {
            throw ClientError.unreachable(error.description)
        }
        switch checkPeer(token, requirement: requirement) {
        case .trusted: return SocketConnection(socket)
        case .untrusted(let status): throw ClientError.untrustedDaemon("code check \(status)")
        case .unknown(let status): throw ClientError.untrustedDaemon("cannot identify it, \(status)")
        }
    }
}

final class SocketConnection: Connection, @unchecked Sendable {
    // UnixSocket's descriptor is used by one thread at a time; cancel only calls shutdown(2),
    // which is safe from any thread while the descriptor stays open.
    private let socket: UnixSocket

    init(_ socket: UnixSocket) { self.socket = socket }

    func write(_ bytes: [UInt8]) throws { try socket.send(Data(bytes)) }
    func read() throws -> [UInt8] { try socket.readSome() }
    func cancel() { socket.shutdownBoth() }
}

let maxHeadBytes = 64 * 1024
let maxMessageBytes = 16 * 1024 * 1024

/// Builds one HTTP/1.1 request. Each call uses its own connection, closed after the answer.
func httpRequest(path: String, contentType: String, body: [UInt8], unary: Bool) -> [UInt8] {
    var head = [
        "POST \(path) HTTP/1.1",
        "Host: localhost",
        "Content-Type: \(contentType)",
        "Content-Length: \(body.count)",
        "Connection: close",
    ]
    if unary { head.append("Connect-Protocol-Version: 1") }
    return Array((head.joined(separator: "\r\n") + "\r\n\r\n").utf8) + body
}

struct HTTPHead: Equatable {
    var status: Int
    var headers: [String: String]
}

/// Reads an HTTP/1.1 response from a connection as it arrives: the head first, then the body in
/// pieces, undoing chunked transfer encoding, so a stream's messages are handled one by one.
struct HTTPReader {
    let connection: any Connection
    private var buffer: [UInt8] = []
    private var closed = false
    private var body = Body.none

    private enum Body {
        case none
        case length(Int)
        case chunked(remaining: Int)
        case untilClose
        case done
    }

    init(_ connection: any Connection) { self.connection = connection }

    private mutating func fill() throws -> Bool {
        guard !closed else { return false }
        let more = try connection.read()
        if more.isEmpty {
            closed = true
            return false
        }
        buffer += more
        return true
    }

    private mutating func line(limit: Int) throws -> String {
        while true {
            if let end = crlf(in: buffer) {
                guard let text = String(bytes: buffer[0..<end], encoding: .utf8) else {
                    throw ClientError.malformed("a line is not UTF-8")
                }
                buffer.removeFirst(end + 2)
                return text
            }
            guard buffer.count <= limit else { throw ClientError.malformed("a line is too long") }
            guard try fill() else { throw ClientError.malformed("the connection closed early") }
        }
    }

    mutating func head() throws -> HTTPHead {
        let status = try line(limit: maxHeadBytes)
        let parts = status.split(separator: " ", maxSplits: 2)
        guard parts.count >= 2, parts[0].hasPrefix("HTTP/1."), let code = Int(parts[1]) else {
            throw ClientError.malformed("no HTTP status line")
        }
        var headers: [String: String] = [:]
        var size = status.utf8.count
        while true {
            let header = try line(limit: maxHeadBytes)
            size += header.utf8.count
            guard size <= maxHeadBytes else { throw ClientError.malformed("the head is too large") }
            if header.isEmpty { break }
            guard let colon = header.firstIndex(of: ":") else { throw ClientError.malformed("a header has no colon") }
            let key = header[..<colon].trimmingCharacters(in: .whitespaces).lowercased()
            headers[key] = header[header.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        if headers["transfer-encoding"]?.lowercased() == "chunked" {
            body = .chunked(remaining: 0)
        } else if let length = headers["content-length"] {
            guard let n = Int(length), n >= 0 else { throw ClientError.malformed("a bad content length") }
            body = .length(n)
        } else {
            body = .untilClose
        }
        return HTTPHead(status: code, headers: headers)
    }

    /// The next piece of the body, or nil at its end.
    mutating func next() throws -> [UInt8]? {
        while true {
            switch body {
            case .none, .done:
                return nil
            case .length(let left):
                if left == 0 {
                    body = .done
                    return nil
                }
                if buffer.isEmpty, try !fill() { throw ClientError.malformed("the body ended early") }
                let piece = Array(buffer.prefix(left))
                buffer.removeFirst(piece.count)
                body = .length(left - piece.count)
                return piece
            case .untilClose:
                if buffer.isEmpty, try !fill() {
                    body = .done
                    return nil
                }
                defer { buffer.removeAll() }
                return buffer
            case .chunked(let remaining):
                if remaining > 0 {
                    if buffer.isEmpty, try !fill() { throw ClientError.malformed("a chunk ended early") }
                    let piece = Array(buffer.prefix(remaining))
                    buffer.removeFirst(piece.count)
                    body = .chunked(remaining: remaining - piece.count)
                    if remaining == piece.count { body = .chunked(remaining: -1) }
                    return piece
                }
                if remaining < 0 {
                    guard try line(limit: 2).isEmpty else { throw ClientError.malformed("a chunk overran its size") }
                }
                let sizeLine = try line(limit: 1024)
                guard
                    let size = Int(
                        sizeLine.split(separator: ";").first?.trimmingCharacters(in: .whitespaces) ?? "",
                        radix: 16), size >= 0
                else { throw ClientError.malformed("a bad chunk size") }
                if size == 0 {
                    body = .done
                    return nil
                }
                body = .chunked(remaining: size)
            }
        }
    }

    /// The whole body, refusing more than limit bytes.
    mutating func all(limit: Int = maxMessageBytes) throws -> [UInt8] {
        var out: [UInt8] = []
        while let piece = try next() {
            out += piece
            guard out.count <= limit else { throw ClientError.malformed("the body is too large") }
        }
        return out
    }
}

private func crlf(in bytes: [UInt8]) -> Int? {
    guard bytes.count >= 2 else { return nil }
    for i in 0..<(bytes.count - 1) where bytes[i] == 13 && bytes[i + 1] == 10 { return i }
    return nil
}

/// One Connect stream envelope: a flag byte, a four byte big endian length, then the message.
struct Envelope: Equatable {
    var flags: UInt8
    var payload: [UInt8]

    static let endStream: UInt8 = 0x02

    static func encode(_ payload: [UInt8]) -> [UInt8] {
        let n = UInt32(payload.count)
        return [0, UInt8(n >> 24), UInt8((n >> 16) & 0xFF), UInt8((n >> 8) & 0xFF), UInt8(n & 0xFF)] + payload
    }
}

/// Splits a stream's body into envelopes however the bytes were cut into chunks and reads.
struct EnvelopeReader {
    private var buffer: [UInt8] = []

    mutating func feed(_ bytes: [UInt8]) { buffer += bytes }

    mutating func next() throws -> Envelope? {
        guard buffer.count >= 5 else { return nil }
        let length = buffer[1...4].reduce(0) { $0 << 8 | Int($1) }
        guard length <= maxMessageBytes else { throw ClientError.malformed("a message is too large") }
        guard buffer.count >= 5 + length else { return nil }
        let envelope = Envelope(flags: buffer[0], payload: Array(buffer[5..<(5 + length)]))
        buffer.removeFirst(5 + length)
        return envelope
    }

    var isEmpty: Bool { buffer.isEmpty }
}

private struct WireError: Decodable {
    struct Detail: Decodable {
        var type: String?
        var value: String?
    }
    var code: String?
    var message: String?
    var details: [Detail]?

    var clientError: ClientError {
        ClientError.refused(code: code ?? "unknown", message: message ?? "", reason: reason)
    }

    /// The berth.v1.ErrorInfo detail's reason (field 1, a varint), when present.
    private var reason: Int? {
        guard let detail = details?.first(where: { $0.type == "berth.v1.ErrorInfo" }),
            let value = detail.value, let bytes = base64(value), bytes.first == 0x08
        else { return nil }
        var result = 0
        for (i, byte) in bytes.dropFirst().prefix(5).enumerated() {
            result |= Int(byte & 0x7F) << (7 * i)
            if byte & 0x80 == 0 { return result }
        }
        return nil
    }
}

private struct EndOfStream: Decodable {
    var error: WireError?
}

/// Connect allows unpadded base64 in error details.
private func base64(_ text: String) -> [UInt8]? {
    var padded = text.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
    padded += String(repeating: "=", count: (4 - padded.count % 4) % 4)
    return Data(base64Encoded: padded).map(Array.init)
}

private func decodeError(_ body: [UInt8], status: Int) -> ClientError {
    (try? JSONDecoder().decode(WireError.self, from: Data(body)))?.clientError ?? .http(status)
}

/// A Connect client for berthd's socket, speaking JSON over HTTP/1.1, one connection per call.
public struct ConnectClient: Sendable {
    public var dialer: any Dialer
    public var unaryTimeout: TimeInterval = 10

    public init(dialer: any Dialer) { self.dialer = dialer }

    public func unary<Request: Encodable, Response: Decodable & Sendable>(
        _ path: String, _ request: Request, as: Response.Type
    ) async throws -> Response {
        let body = try Array(JSONEncoder().encode(request))
        let dialer = dialer
        let timeout = unaryTimeout
        return try await onThread {
            let connection = try dialer.dial(timeout: timeout)
            defer { connection.cancel() }
            try connection.write(httpRequest(path: path, contentType: "application/json", body: body, unary: true))
            var reader = HTTPReader(connection)
            let head = try reader.head()
            let answer = try reader.all()
            guard head.status == 200 else { throw decodeError(answer, status: head.status) }
            do {
                return try JSONDecoder().decode(Response.self, from: Data(answer))
            } catch {
                throw ClientError.malformed("the answer does not decode")
            }
        }
    }

    /// A server stream. It finishes when berthd ends the stream cleanly, throws a ClientError
    /// when it ends with an error, and stops reading as soon as the consumer stops iterating.
    public func serverStream<Request: Encodable, Response: Decodable & Sendable>(
        _ path: String, _ request: Request, as: Response.Type
    ) -> AsyncThrowingStream<Response, any Error> {
        let dialer = dialer
        let encoded = Result { try Array(JSONEncoder().encode(request)) }
        return AsyncThrowingStream { continuation in
            let cancel = Cancel()
            continuation.onTermination = { _ in cancel.fire() }
            Thread.detachNewThread {
                do {
                    let body = Envelope.encode(try encoded.get())
                    let connection = try dialer.dial(timeout: nil)
                    defer { connection.cancel() }
                    guard cancel.hold(connection) else { return }
                    try connection.write(
                        httpRequest(path: path, contentType: "application/connect+json", body: body, unary: false))
                    try readStream(connection, into: continuation)
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: cancel.fired ? CancellationError() : error)
                }
            }
        }
    }
}

private func readStream<Response: Decodable & Sendable>(
    _ connection: any Connection, into continuation: AsyncThrowingStream<Response, any Error>.Continuation
) throws {
    var reader = HTTPReader(connection)
    let head = try reader.head()
    guard head.status == 200 else { throw decodeError(try reader.all(), status: head.status) }
    var envelopes = EnvelopeReader()
    while let piece = try reader.next() {
        envelopes.feed(piece)
        while let envelope = try envelopes.next() {
            if envelope.flags & Envelope.endStream != 0 {
                let end = try? JSONDecoder().decode(EndOfStream.self, from: Data(envelope.payload))
                guard let end else { throw ClientError.malformed("a bad end of stream") }
                if let error = end.error { throw error.clientError }
                return
            }
            guard envelope.flags == 0 else { throw ClientError.malformed("a compressed or unknown message") }
            guard let message = try? JSONDecoder().decode(Response.self, from: Data(envelope.payload)) else {
                throw ClientError.malformed("a message does not decode")
            }
            continuation.yield(message)
        }
    }
    throw ClientError.malformed("the stream ended without an end of stream message")
}

/// Lets a consumer that stops iterating end a read blocked on another thread.
private final class Cancel: @unchecked Sendable {
    private let lock = NSLock()
    private var connection: (any Connection)?
    private var done = false

    var fired: Bool { lock.withLock { done } }

    func fire() {
        let held = lock.withLock {
            done = true
            return connection
        }
        held?.cancel()
    }

    /// Records the connection to cancel; false when the consumer already went away.
    func hold(_ connection: any Connection) -> Bool {
        lock.withLock {
            self.connection = connection
            return !done
        }
    }
}

/// Runs blocking socket work on its own thread, so it never holds a cooperative pool thread.
func onThread<T: Sendable>(_ body: @escaping @Sendable () throws -> T) async throws -> T {
    try await withCheckedThrowingContinuation { continuation in
        Thread.detachNewThread { continuation.resume(with: Result { try body() }) }
    }
}
