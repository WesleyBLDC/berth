import BerthNativeCore
import Foundation
import Network
import Security

func readFailed(_ message: String) -> Never {
    diagnose(message)
    exit(Exit.readFailed)
}

func runRead(_ line: Data) -> Never {
    let ticket: [UInt8]
    do {
        ticket = try parseTicket(line)
    } catch {
        readFailed(error.description)
    }
    let path = socketPath(environment: ProcessInfo.processInfo.environment, fallbackHome: NSHomeDirectory())
    let name: String
    do {
        let raw = try exchange(path: path, request: redeemRequest(ticket: ticket, requestID: randomHex()))
        name = try redeemedName(from: parseHTTPResponse(raw))
    } catch {
        readFailed("\(error)")
    }
    let result = Keychain.withValue(name) { bytes in writeAll(bytes) }
    switch result {
    case .success(true): exit(0)
    case .success(false): readFailed("could not write the value to stdout")
    case .failure(let error): readFailed("\(keychainService(for: name)): \(error.description)")
    }
}

private func writeAll(_ bytes: UnsafeRawBufferPointer) -> Bool {
    guard var next = bytes.baseAddress else { return true }
    var left = bytes.count
    while left > 0 {
        let written = write(STDOUT_FILENO, next, left)
        if written < 0 {
            if errno == EINTR { continue }
            return false
        }
        next += written
        left -= written
    }
    return true
}

private func randomHex() -> String {
    var bytes = [UInt8](repeating: 0, count: 16)
    _ = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
    return hexString(bytes)
}

struct ExchangeError: Error, CustomStringConvertible {
    var description: String
}

/// One HTTP/1.1 request and its whole response over the daemon's Unix socket.
func exchange(path: String, request: Data, timeout: TimeInterval = 10) throws(ExchangeError) -> Data {
    let call = SocketCall(path: path)
    guard let response = call.run(request, timeout: timeout) else {
        throw ExchangeError(description: call.failure ?? "no response from \(path)")
    }
    return response
}

private final class SocketCall: @unchecked Sendable {
    private static let maxResponse = 64 * 1024
    private let connection: NWConnection
    private let queue = DispatchQueue(label: "berth-native.read")
    private let done = DispatchSemaphore(value: 0)
    private let lock = NSLock()
    private var received = Data()
    private var finished = false
    private var connected = false
    private(set) var failure: String?

    init(path: String) {
        connection = NWConnection(to: .unix(path: path), using: .tcp)
    }

    func run(_ request: Data, timeout: TimeInterval) -> Data? {
        connection.stateUpdateHandler = { [self] state in
            switch state {
            case .ready:
                lock.withLock { connected = true }
                connection.send(
                    content: request,
                    completion: .contentProcessed { [self] error in
                        if let error { finish("send: \(error)") } else { receive() }
                    })
            // A local socket that is not there will not appear later, so waiting is a failure.
            // Once connected, the peer closing moves the state too, and the receive chain must
            // still deliver the response it already holds.
            case .waiting(let error), .failed(let error):
                if !lock.withLock({ connected }) { finish("connect \(connection.endpoint): \(error)") }
            default: break
            }
        }
        connection.start(queue: queue)
        let timedOut = done.wait(timeout: .now() + timeout) == .timedOut
        connection.cancel()
        return lock.withLock {
            if timedOut { failure = "timed out" }
            return failure == nil ? received : nil
        }
    }

    private func receive() {
        connection.receive(minimumIncompleteLength: 1, maximumLength: Self.maxResponse) {
            [self] data, _, complete, error in
            let tooBig = lock.withLock {
                if let data { received.append(data) }
                return received.count > Self.maxResponse
            }
            if tooBig {
                finish("response too large")
            } else if let error {
                finish("receive: \(error)")
            } else if complete {
                finish(nil)
            } else {
                receive()
            }
        }
    }

    private func finish(_ error: String?) {
        let first = lock.withLock {
            guard !finished else { return false }
            finished = true
            failure = error
            return true
        }
        if first { done.signal() }
    }
}
