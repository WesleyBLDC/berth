import Foundation
import Testing

@testable import BerthAppCore

/// Each watch call takes the next script: messages to send, then an end or an error.
private final class ScriptedSource: SessionSource, @unchecked Sendable {
    struct Script {
        var messages: [WatchSessionsResponse]
        var error: ClientError?
    }
    private let lock = NSLock()
    private var scripts: [Script]
    private(set) var afters: [Int64] = []

    init(_ scripts: [Script]) { self.scripts = scripts }

    func watch(after: Int64) -> AsyncThrowingStream<WatchSessionsResponse, any Error> {
        let script = lock.withLock {
            afters.append(after)
            return scripts.isEmpty ? Script(messages: [], error: .unreachable("no berthd")) : scripts.removeFirst()
        }
        return AsyncThrowingStream { continuation in
            for m in script.messages { continuation.yield(m) }
            continuation.finish(throwing: script.error)
        }
    }

    var calls: [Int64] { lock.withLock { afters } }
}

private final class Sleeps: @unchecked Sendable {
    private let lock = NSLock()
    private var list: [Duration] = []
    let limit: Int

    init(limit: Int) { self.limit = limit }

    func sleep(_ d: Duration) throws {
        try lock.withLock {
            list.append(d)
            if list.count >= limit { throw CancellationError() }
        }
    }

    var all: [Duration] { lock.withLock { list } }
}

private let change = WatchSessionsResponse.change(
    SessionChange(eventID: 42, event: "Seen", session: Session(id: "7-8", state: .discovered)))

@MainActor @Test func itResumesAfterTheLastEventAndBacksOff() async {
    let source = ScriptedSource([
        .init(messages: [.snapshot(SessionsSnapshot(eventID: 40, sessions: [], reason: .start)), change], error: nil),
        .init(messages: [], error: .unreachable("refused\u{1B}[2J")),
        .init(messages: [], error: .unreachable("refused")),
        .init(messages: [], error: .unreachable("refused")),
        .init(messages: [], error: .unreachable("refused")),
        .init(messages: [], error: .unreachable("refused")),
        .init(messages: [change], error: nil),
        .init(messages: [], error: .unreachable("refused")),
    ])
    let sleeps = Sleeps(limit: 8)
    let model = SessionsModel(source: source, sleep: { try sleeps.sleep($0) })
    await model.run()
    #expect(source.calls == [0, 42, 42, 42, 42, 42, 42, 42])
    #expect(sleeps.all == [1, 2, 4, 8, 10, 10, 1, 2].map { Duration.seconds($0) })
    #expect(model.store.sessions["7-8"]?.state == .discovered)
    #expect(model.link == .reconnecting("berthd is not reachable: refused"))
}

@MainActor @Test func theReasonIsEscaped() async {
    let source = ScriptedSource([.init(messages: [], error: .unreachable("x\u{1B}[2J"))])
    let sleeps = Sleeps(limit: 1)
    let model = SessionsModel(source: source, sleep: { try sleeps.sleep($0) })
    await model.run()
    #expect(model.link == .reconnecting(#""berthd is not reachable: x\x1b[2J""#))
}
