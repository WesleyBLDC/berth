import BerthNativeCore
import Foundation
import Security

func readFailed(_ message: String) -> Never {
    diagnose(message)
    exit(Exit.readFailed)
}

func runRead(_ line: Data) -> Never {
    // The receiver is checked before the ticket is spent: the value goes only to the signed
    // berth or berth-proxy that started this helper, over the socket it handed over.
    let parent: ConsumerKind
    switch checkValueChannel(valueFD, parent: getppid(), requirements: parentRequirements) {
    case .parent(let kind): parent = kind
    case .refused(let why): readFailed(why)
    }
    let ticket: [UInt8]
    do {
        ticket = try parseTicket(line)
    } catch {
        readFailed(error.description)
    }
    let redeemed: RedeemedTicket
    do {
        redeemed = try redeem(ticket)
    } catch {
        readFailed("\(error)")
    }
    if let refusal = useRefusal(parent: parent, redeemed) { readFailed(refusal) }
    let name = redeemed.name
    let result = Keychain.withValue(name) { bytes in writeAll(bytes) }
    switch result {
    case .success(true): exit(0)
    case .success(false): readFailed("could not write the value to the value channel")
    case .failure(let error): readFailed("\(keychainService(for: name)): \(error.description)")
    }
}

/// Redeems a ticket with the signed berthd at the daemon socket.
func redeem(_ ticket: [UInt8]) throws -> RedeemedTicket {
    let path = socketPath(environment: ProcessInfo.processInfo.environment, fallbackHome: NSHomeDirectory())
    let raw = try exchange(path: path, request: redeemRequest(ticket: ticket, requestID: randomHex()))
    return try redeemedTicket(from: parseHTTPResponse(raw))
}

private func writeAll(_ bytes: UnsafeRawBufferPointer) -> Bool {
    guard var next = bytes.baseAddress else { return true }
    var left = bytes.count
    while left > 0 {
        let written = write(valueFD, next, left)
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

/// One HTTP/1.1 request and its whole response over the daemon's Unix socket, sent only after
/// the kernel shows the process answering is the signed berthd: anyone can listen on a socket
/// at BERTH_HOME, so the ticket must never reach a server an agent started.
func exchange(path: String, request: Data) throws(ExchangeError) -> Data {
    do {
        let socket = try UnixSocket(path: path)
        let token = try socket.peerAuditToken()
        switch checkPeer(token, requirement: berthDaemonRequirement) {
        case .trusted: break
        case .untrusted(let status):
            throw ExchangeError(description: "the server on \(path) is not the signed berthd (\(status))")
        case .unknown(let status): throw ExchangeError(description: "cannot identify the server on \(path) (\(status))")
        }
        try socket.send(request)
        return try socket.readAll(limit: 64 * 1024)
    } catch let error as SocketError {
        throw ExchangeError(description: error.description)
    } catch let error as ExchangeError {
        throw error
    } catch {
        throw ExchangeError(description: "\(error)")
    }
}
