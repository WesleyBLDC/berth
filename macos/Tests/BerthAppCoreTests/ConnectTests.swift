import BerthSocket
import Darwin
import Foundation
import Testing

@testable import BerthAppCore

private func client(_ connection: FakeConnection) -> DaemonClient {
    DaemonClient(connect: ConnectClient(dialer: FakeDialer(connection: connection)), clientName: "test")
}

private func unaryAnswer(status: String = "200 OK", _ body: String) -> String {
    "HTTP/1.1 \(status)\r\nContent-Type: application/json\r\nContent-Length: \(body.utf8.count)\r\n\r\n\(body)"
}

@Test func unaryCallSendsExactlyOneConnectRequest() async throws {
    let connection = FakeConnection(unaryAnswer(#"{"protocolVersion":1,"daemonVersion":"dev"}"#), cutEvery: 3)
    let answer = try await client(connection).handshake()
    #expect(answer.daemonVersion == "dev")
    let body = #"{"clientName":"test","protocolVersion":1}"#
    let sent = connection.writtenText
    #expect(sent.hasPrefix("POST /berth.v1.DaemonService/Handshake HTTP/1.1\r\n"))
    #expect(sent.contains("\r\nContent-Type: application/json\r\n"))
    #expect(sent.contains("\r\nConnect-Protocol-Version: 1\r\n"))
    #expect(sent.contains("\r\nConnection: close\r\n"))
    #expect(sent.contains("\r\nContent-Length: \(body.utf8.count)\r\n"))
    let sentBody = try JSONSerialization.jsonObject(with: Data(sent.components(separatedBy: "\r\n\r\n")[1].utf8))
    #expect((sentBody as? [String: AnyHashable]) == ["clientName": "test", "protocolVersion": 1])
}

@Test func connectErrorsCarryTheTypedReason() async {
    // berth.v1.ErrorInfo{reason: 200}: field 1 varint, two bytes, unpadded base64 as Connect may send.
    let body =
        #"{"code":"permission_denied","message":"not for agents","details":[{"type":"berth.v1.ErrorInfo","value":"CMgB"}]}"#
    let connection = FakeConnection(unaryAnswer(status: "403 Forbidden", body))
    await #expect(throws: ClientError.refused(code: "permission_denied", message: "not for agents", reason: 200)) {
        try await client(connection).handshake()
    }
}

@Test func anotherProtocolVersionIsRefused() async {
    let connection = FakeConnection(unaryAnswer(#"{"protocolVersion":2}"#))
    await #expect(throws: ClientError.protocolMismatch(2)) { try await client(connection).handshake() }
}

private let snapshotJSON = #"{"snapshot":{"eventId":"10","sessions":[{"id":"1-2","state":"SESSION_STATE_IDLE"}]}}"#
private let changeJSON =
    #"{"change":{"eventId":"12","event":"Busy","session":{"id":"1-2","state":"SESSION_STATE_ACTIVE"}}}"#

private func stream(_ pieces: [[UInt8]], holdOpen: Bool = false) -> (
    FakeConnection, AsyncThrowingStream<WatchSessionsResponse, any Error>
) {
    let connection = FakeConnection(pieces, holdOpen: holdOpen)
    let s = ConnectClient(dialer: FakeDialer(connection: connection)).serverStream(
        "/berth.v1.SessionsService/WatchSessions", WatchSessionsRequest(afterEventId: ProtoInt64(9)),
        as: WatchSessionsResponse.self)
    return (connection, s)
}

private func collect(_ s: AsyncThrowingStream<WatchSessionsResponse, any Error>) async throws -> [WatchSessionsResponse]
{
    var out: [WatchSessionsResponse] = []
    for try await m in s { out.append(m) }
    return out
}

private func cut(_ bytes: [UInt8], every size: Int) -> [[UInt8]] {
    stride(from: 0, to: bytes.count, by: size).map { Array(bytes[$0..<min($0 + size, bytes.count)]) }
}

@Test func streamMessagesSurviveAnyCutOfChunksAndReads() async throws {
    let body = envelope(snapshotJSON) + envelope(changeJSON) + envelope("{}", flags: 2)
    for (chunk, read) in [(7, 5), (1, 1), (1000, 1000), (13, 64)] {
        let (connection, s) = stream(cut(chunkedAnswer(body, chunk: chunk), every: read))
        let messages = try await collect(s)
        #expect(messages.count == 2)
        guard case .change(let change) = messages.last else {
            Issue.record("no change")
            continue
        }
        #expect(change.eventID == 12)
        let sent = connection.writtenText
        #expect(sent.hasPrefix("POST /berth.v1.SessionsService/WatchSessions HTTP/1.1\r\n"))
        #expect(sent.contains("\r\nContent-Type: application/connect+json\r\n"))
        #expect(!sent.contains("Connect-Protocol-Version"))
        let request = Array(connection.writtenText.utf8).split(separator: 10).last.map(Array.init) ?? []
        #expect(request.prefix(5) == [0, 0, 0, 0, 20])
        #expect(String(decoding: request.dropFirst(5), as: UTF8.self) == #"{"afterEventId":"9"}"#)
    }
}

@Test func anEndOfStreamErrorIsThrown() async {
    let end = #"{"error":{"code":"resource_exhausted","message":"too many streams"}}"#
    let (_, s) = stream([chunkedAnswer(envelope(snapshotJSON) + envelope(end, flags: 2), chunk: 9)])
    await #expect(throws: ClientError.refused(code: "resource_exhausted", message: "too many streams", reason: nil)) {
        _ = try await collect(s)
    }
}

@Test func brokenStreamsEndWithAnError() async {
    let head = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
    var huge = envelope("{}")
    huge.replaceSubrange(1...4, with: [0x7F, 0xFF, 0xFF, 0xFF])
    let cases: [[UInt8]] = [
        Array((head + "zz\r\nabc\r\n0\r\n\r\n").utf8),
        chunkedAnswer(huge, chunk: 100),
        chunkedAnswer(envelope(snapshotJSON), chunk: 100),
        Array((head + "5\r\nab").utf8),
        chunkedAnswer(envelope("{\"snapshot\":", flags: 0) + envelope("{}", flags: 2), chunk: 100),
        chunkedAnswer(envelope("{}", flags: 1), chunk: 100),
        Array("HTTP/2 200\r\n\r\n".utf8),
    ]
    for bytes in cases {
        let (_, s) = stream([bytes])
        await #expect(throws: ClientError.self) { _ = try await collect(s) }
    }
}

/// Where the app once read a stream differently from connect-go, found by the Go and Swift corpus.
@Test func streamsReadAsConnectGoReadsThem() async throws {
    let end = envelope("{}", flags: 2)
    let refused = envelope(#"{"error":{"code":"permission_denied","message":"no"}}"#, flags: 2)
    for body in [
        end + envelope(snapshotJSON), end + Array("junk".utf8), end + refused, refused + end, envelope("{}", flags: 3),
    ] {
        let (_, s) = stream([chunkedAnswer(body, chunk: 5)])
        do {
            _ = try await collect(s)
            Issue.record("a stream broken after its end passed")
        } catch let error as ClientError {
            guard case .malformed = error else {
                Issue.record("a stream broken after its end gave \(error)")
                continue
            }
        }
    }
    let (_, empty) = stream([chunkedAnswer(envelope("") + end, chunk: 100)])
    #expect(try await collect(empty) == [.unknown])
    let (_, unknown) = stream([
        chunkedAnswer(envelope(#"{"error":{"code":"bogus","message":"no"}}"#, flags: 2), chunk: 100)
    ])
    await #expect(throws: ClientError.refused(code: "unknown", message: "no", reason: nil)) {
        _ = try await collect(unknown)
    }
}

@Test func aStreamRefusedUpFrontCarriesTheError() async {
    let (_, s) = stream([Array(unaryAnswer(status: "429 Too Many Requests", #"{"code":"resource_exhausted"}"#).utf8)])
    await #expect(throws: ClientError.refused(code: "resource_exhausted", message: "", reason: nil)) {
        _ = try await collect(s)
    }
}

@Test func stoppingIterationEndsABlockedRead() async throws {
    let head = Array("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n".utf8)
    let first = envelope(snapshotJSON)
    let connection = FakeConnection(
        [head + Array((String(first.count, radix: 16) + "\r\n").utf8) + first], holdOpen: true)
    // The stream lives only inside this call, as in SessionsModel, so leaving the loop ends it.
    func firstMessage() async throws -> WatchSessionsResponse? {
        let s = ConnectClient(dialer: FakeDialer(connection: connection)).serverStream(
            "/berth.v1.SessionsService/WatchSessions", WatchSessionsRequest(afterEventId: ProtoInt64(0)),
            as: WatchSessionsResponse.self)
        for try await m in s { return m }
        return nil
    }
    #expect(try await firstMessage() != nil)
    let deadline = ContinuousClock.now + .seconds(5)
    while !connection.isCancelled && ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(10)) }
    #expect(connection.isCancelled)
}

/// The real dialer asks the kernel who answers before writing anything: this test process is
/// not the signed berthd, so the dial fails and the server reads nothing.
@Test func theRealDialerWritesNothingToAnUnsignedServer() throws {
    let path = NSTemporaryDirectory() + "berth-app-\(getpid()).sock"
    unlink(path)
    let server = socket(AF_UNIX, SOCK_STREAM, 0)
    #expect(server >= 0)
    defer {
        close(server)
        unlink(path)
    }
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    withUnsafeMutableBytes(of: &address.sun_path) { raw in
        raw.copyBytes(from: path.utf8)
        raw[path.utf8.count] = 0
    }
    let bound = withUnsafePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            bind(server, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    #expect(bound == 0)
    #expect(listen(server, 1) == 0)
    #expect {
        _ = try SocketDialer(path: path).dial(timeout: 1)
    } throws: { error in
        guard case ClientError.untrustedDaemon = error else { return false }
        return true
    }
    let accepted = accept(server, nil, nil)
    #expect(accepted >= 0)
    defer { close(accepted) }
    var byte: UInt8 = 0
    #expect(read(accepted, &byte, 1) == 0)
}

/// The Go test writes the requirement Go pins berthd by in release builds; the app must use
/// exactly that one.
@Test func theDaemonRequirementMatchesGo() throws {
    let go = try JSONDecoder().decode([String: String].self, from: fixture("requirement.json"))
    #expect(go["daemon"] == berthDaemonRequirement)
    #expect(SocketDialer(path: "/x").requirement == berthDaemonRequirement)
}
