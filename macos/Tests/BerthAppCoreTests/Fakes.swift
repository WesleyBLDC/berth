import Foundation

@testable import BerthAppCore

/// A scripted connection: each read returns the next piece, then the peer closes. With holdOpen
/// the last read blocks until cancel, like a quiet stream.
final class FakeConnection: Connection, @unchecked Sendable {
    private let lock = NSCondition()
    private var pieces: [[UInt8]]
    private var cancelled = false
    private let holdOpen: Bool
    private(set) var written: [UInt8] = []

    init(_ pieces: [[UInt8]], holdOpen: Bool = false) {
        self.pieces = pieces
        self.holdOpen = holdOpen
    }

    convenience init(_ text: String, cutEvery size: Int = 0) {
        let bytes = Array(text.utf8)
        self.init(
            size == 0
                ? [bytes]
                : stride(from: 0, to: bytes.count, by: size).map {
                    Array(bytes[$0..<min($0 + size, bytes.count)])
                })
    }

    var isCancelled: Bool { lock.withLock { cancelled } }

    var writtenText: String { lock.withLock { String(decoding: written, as: UTF8.self) } }

    func write(_ bytes: [UInt8]) throws { lock.withLock { written += bytes } }

    func read() throws -> [UInt8] {
        lock.lock()
        defer { lock.unlock() }
        while pieces.isEmpty && holdOpen && !cancelled { lock.wait() }
        if cancelled || pieces.isEmpty { return [] }
        return pieces.removeFirst()
    }

    func cancel() {
        lock.withLock {
            cancelled = true
            lock.broadcast()
        }
    }
}

struct FakeDialer: Dialer {
    var connection: FakeConnection?
    var refusal: ClientError?

    func dial(timeout: TimeInterval?) throws -> any Connection {
        if let refusal { throw refusal }
        guard let connection else { throw ClientError.unreachable("no connection") }
        return connection
    }
}

func envelope(_ json: String, flags: UInt8 = 0) -> [UInt8] {
    var bytes = Envelope.encode(Array(json.utf8))
    bytes[0] = flags
    return bytes
}

/// A chunked HTTP/1.1 stream answer whose body is the given bytes, cut into chunks of size.
func chunkedAnswer(_ body: [UInt8], chunk size: Int) -> [UInt8] {
    var out = Array(
        "HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nTransfer-Encoding: chunked\r\n\r\n".utf8)
    for start in stride(from: 0, to: body.count, by: size) {
        let piece = body[start..<min(start + size, body.count)]
        out += Array((String(piece.count, radix: 16) + "\r\n").utf8) + piece + Array("\r\n".utf8)
    }
    return out + Array("0\r\n\r\n".utf8)
}

func fixture(_ name: String) throws -> Data {
    try Data(
        contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .appendingPathComponent("Fixtures").appendingPathComponent(name))
}
