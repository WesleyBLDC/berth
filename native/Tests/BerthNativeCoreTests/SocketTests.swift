import BerthSocket
import Darwin
import Foundation
import Testing

@testable import BerthNativeCore

/// The test process holds both ends of a socketpair, so it is the peer; with parent set to
/// this process, only the requirement decides.
@Test func valueChannelMustBeASocketToTheSignedParent() throws {
    var pair: [Int32] = [0, 0]
    #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
    var pipeFDs: [Int32] = [0, 0]
    #expect(pipe(&pipeFDs) == 0)
    let path = NSTemporaryDirectory() + "berth-not-a-socket-\(getpid())"
    let file = open(path, O_CREAT | O_WRONLY, 0o600)
    let unconnected = socket(AF_UNIX, SOCK_STREAM, 0)
    defer {
        for fd in pair + pipeFDs + [file, unconnected] { close(fd) }
        unlink(path)
    }
    let me = getpid()
    let always = [(ConsumerKind.executable, "always")]
    #expect(checkValueChannel(pair[1], parent: me, requirements: always) == .parent(.executable))
    // The test binary is neither signed parent, so the real pair refuses it.
    guard case .refused(let unsigned) = checkValueChannel(pair[1], parent: me, requirements: parentRequirements)
    else {
        Issue.record("an unsigned peer passed the parent requirements")
        return
    }
    #expect(unsigned.contains("not the signed berth or berth-proxy"))
    #expect(refusal(pair[1], parent: me + 1)?.contains("is not the parent") == true)
    #expect(refusal(pipeFDs[1], parent: me) == "the value channel is not a socket")
    #expect(refusal(file, parent: me) == "the value channel is not a socket")
    let unconnectedRefusal = refusal(unconnected, parent: me)
    #expect(
        unconnectedRefusal?.contains("has no peer") == true,
        "fd \(unconnected) refused with \(String(describing: unconnectedRefusal))")
    #expect(refusal(999, parent: me) == "no value channel on fd 999")
}

/// The kind a parent proves comes from the requirement it satisfied, not from the order tried.
@Test func valueChannelSaysWhichParentRequirementPassed() throws {
    var pair: [Int32] = [0, 0]
    #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
    defer { for fd in pair { close(fd) } }
    let proxyOnly = [(ConsumerKind.executable, berthCLIRequirement), (ConsumerKind.proxy, "always")]
    #expect(checkValueChannel(pair[1], parent: getpid(), requirements: proxyOnly) == .parent(.proxy))
}

private func refusal(_ fd: Int32, parent: pid_t) -> String? {
    guard case .refused(let why) = checkValueChannel(fd, parent: parent, requirements: [(.executable, "always")])
    else { return nil }
    return why
}

/// Serves one connection on a temporary socket from this process, answering with reply.
private func serveOnce(reply: Data) throws -> String {
    let dir = NSTemporaryDirectory() + "bs\(getpid())\(Int.random(in: 0..<1_000_000))"
    try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
    let path = dir + "/s.sock"
    let listener = socket(AF_UNIX, SOCK_STREAM, 0)
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    withUnsafeMutableBytes(of: &address.sun_path) { raw in
        raw.copyBytes(from: path.utf8)
        raw[path.utf8.count] = 0
    }
    let bound = withUnsafePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    #expect(bound == 0)
    #expect(listen(listener, 1) == 0)
    Thread.detachNewThread {
        let conn = accept(listener, nil, nil)
        var buffer = [UInt8](repeating: 0, count: 1024)
        _ = read(conn, &buffer, buffer.count)
        reply.withUnsafeBytes { _ = write(conn, $0.baseAddress, $0.count) }
        close(conn)
        close(listener)
    }
    return path
}

@Test func socketExchangeKnowsWhoAnswers() throws {
    let path = try serveOnce(reply: Data("pong".utf8))
    let socket = try UnixSocket(path: path)
    let token = try socket.peerAuditToken()
    // The server is this test process: its pid is the token's sixth word.
    #expect(Int32(bitPattern: token.val.5) == getpid())
    // The test binary is not berthd signed by Berth's team, so berth-native would refuse it.
    guard case .untrusted = checkPeer(token, requirement: berthDaemonRequirement) else {
        Issue.record("an unsigned server passed the daemon requirement")
        return
    }
    try socket.send(Data("ping".utf8))
    #expect(try socket.readAll(limit: 1024) == Data("pong".utf8))
}

@Test func missingSocketIsAnError() {
    #expect(throws: SocketError.self) { try UnixSocket(path: NSTemporaryDirectory() + "berth-missing.sock") }
}

@Test func daemonRequirementPinsTheSignedDaemon() {
    #expect(berthDaemonRequirement.hasPrefix("anchor apple generic"))
    #expect(berthDaemonRequirement.contains(#"identifier "com.wesleybldc.berth.daemon""#))
}
