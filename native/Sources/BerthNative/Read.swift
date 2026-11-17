import BerthNativeCore
import Foundation
import Security

func readFailed(_ message: String) -> Never {
    diagnose(message)
    exit(Exit.readFailed)
}

func runRead(_ line: Data) -> Never {
    // The value goes only into the pipe berth run reads, never a file or terminal a wrapper
    // could have put on stdout instead.
    guard isPipe(STDOUT_FILENO) else { readFailed("stdout is not a pipe") }
    let ticket: [UInt8]
    do {
        ticket = try parseTicket(line)
    } catch {
        readFailed(error.description)
    }
    // Defence in depth: berthd only issues tickets to the signed berth CLI, and this keeps a
    // ticket from working in a helper that any other process started.
    if let refusal = parentRefusal() { readFailed(refusal) }
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

/// nil when the parent process is the signed berth CLI. The parent is found by pid, so the pid is
/// read again afterwards: had the parent exited and its pid been reused, this process would
/// have been reparented and getppid would differ.
private func parentRefusal() -> String? {
    let parent = getppid()
    var code: SecCode?
    let attributes = [kSecGuestAttributePid: NSNumber(value: parent)] as CFDictionary
    var status = SecCodeCopyGuestWithAttributes(nil, attributes, [], &code)
    guard status == errSecSuccess, let code else { return "cannot identify the parent process (\(status))" }
    var requirement: SecRequirement?
    status = SecRequirementCreateWithString(berthCLIRequirement as CFString, [], &requirement)
    guard status == errSecSuccess, let requirement else { return "cannot build the parent requirement (\(status))" }
    status = SecCodeCheckValidity(code, [], requirement)
    guard status == errSecSuccess else { return "the parent process is not the signed berth CLI (\(status))" }
    guard getppid() == parent else { return "the parent process changed during the check" }
    return nil
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
