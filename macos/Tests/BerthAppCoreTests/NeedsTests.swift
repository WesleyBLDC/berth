import Foundation
import Testing

@testable import BerthAppCore

/// The Go test wrote this from WatchNeedsResponse messages that set every Need field.
@Test func needsFixtureDecodesEveryField() throws {
    let messages = try JSONDecoder().decode([WatchNeedsResponse].self, from: fixture("needs.json"))
    guard messages.count == 4, case .snapshot(let snapshot) = messages[0], case .change(let opened) = messages[1],
        case .change(let resolved) = messages[2], case .cursor(let cursor) = messages[3]
    else {
        Issue.record("unexpected messages \(messages)")
        return
    }
    #expect(snapshot.eventID == 9_007_199_254_740_993)
    #expect(snapshot.reason == .behind)
    let need = try #require(snapshot.needs.first)
    #expect(need.id == "session/4242-1763380000123456")
    #expect(need.kind == .leftover)
    #expect(need.secretName == "gh-token")
    #expect(need.ownerName == "node\u{202E}")
    #expect(need.browser == .headlessShell)
    #expect(opened.eventID == 9_007_199_254_740_995)
    #expect(opened.event == .opened)
    #expect(opened.need == need)
    #expect(resolved.event == .resolved)
    #expect(resolved.need == Need(id: "grant/7", kind: .grant, secretName: "gh-token"))
    #expect(cursor == 9_007_199_254_740_999)
}

/// berthd sends the cursor past rows that changed no need, so a reconnect resumes after them.
@Test func aCursorMessageOnlyMovesTheCursor() {
    var s = NeedsStore()
    #expect(s.apply(.change(NeedChange(eventID: 6, event: .opened, need: store)), posting: true) == [.post(store)])
    #expect(s.apply(.cursor(40), posting: true) == [])
    #expect(s.cursor == 40)
    #expect(s.open.keys.sorted() == [store.id])
}

/// A need whose facts change while open is posted again under its id, replacing the old text.
@Test func aChangedNeedIsPostedAgain() {
    var s = NeedsStore()
    #expect(s.apply(.change(NeedChange(eventID: 6, event: .opened, need: store)), posting: true) == [.post(store)])
    let rotate = Need(id: store.id, kind: .rotate, secretName: "gh-token")
    #expect(s.apply(.change(NeedChange(eventID: 7, event: .opened, need: rotate)), posting: true) == [.post(rotate)])
    #expect(s.apply(.snapshot(NeedsSnapshot(eventID: 8, needs: [rotate], reason: .behind)), posting: true) == [])
}

@Test func unknownNeedNamesDoNotFail() throws {
    let json = #"[{"change":{"eventId":"3","event":"NEED_EVENT_NEW","need":{"id":"x/1","kind":"NEED_KIND_NEW"}}}]"#
    let messages = try JSONDecoder().decode([WatchNeedsResponse].self, from: Data(json.utf8))
    #expect(
        messages == [.change(NeedChange(eventID: 3, event: .unspecified, need: Need(id: "x/1", kind: .unspecified)))])
}

private let store = Need(id: "secret/gh-token", kind: .store, secretName: "gh-token")
private let leftover = Need(id: "session/1-2", kind: .leftover, browser: .chromeForTesting)

@Test func aNeedIsPostedOnceWhileOpen() {
    var s = NeedsStore()
    #expect(
        s.apply(.snapshot(NeedsSnapshot(eventID: 5, needs: [leftover], reason: .start)), posting: true) == [
            .post(leftover)
        ])
    #expect(s.apply(.change(NeedChange(eventID: 6, event: .opened, need: store)), posting: true) == [.post(store)])
    // A repeat, or a snapshot that shows the same needs, posts nothing.
    #expect(s.apply(.change(NeedChange(eventID: 7, event: .opened, need: store)), posting: true) == [])
    #expect(
        s.apply(.snapshot(NeedsSnapshot(eventID: 8, needs: [store, leftover], reason: .behind)), posting: true) == [])
    #expect(
        s.apply(.change(NeedChange(eventID: 9, event: .resolved, need: store)), posting: true) == [.remove(store.id)])
    #expect(s.open.keys.sorted() == [leftover.id])
    // A snapshot without the leftover removes its notification.
    #expect(
        s.apply(.snapshot(NeedsSnapshot(eventID: 10, needs: [], reason: .unknownCursor)), posting: true) == [
            .remove(leftover.id)
        ])
    #expect(s.cursor == 10)
    #expect(s.apply(.unknown, posting: true) == [])
}

@Test func postingOffOnlyFollowsTheNeeds() {
    var s = NeedsStore()
    #expect(s.apply(.change(NeedChange(eventID: 6, event: .opened, need: store)), posting: false) == [])
    #expect(s.unposted() == [.post(store)])
    #expect(s.forgetPosted() == [.remove(store.id)])
    #expect(s.forgetPosted() == [])
    #expect(s.cursor == 6)
}

@Test func notificationTextQuotesWhatOthersChose() {
    let store = NeedText(Need(id: "secret/x", kind: .store, secretName: "gh-token"))
    #expect(store.title == "A secret is waiting to be stored")
    #expect(store.body == #"The store dialog for secret "gh-token" (name chosen by the requester) is open."#)
    #expect(
        NeedText(Need(id: "grant/1", kind: .grant, secretName: "a.b")).body
            == #"Granting secret "a.b" (name chosen by the requester) needs Touch ID."#)
    #expect(NeedText(Need(id: "secret/x", kind: .rotate, secretName: "x")).title == "A rotate is waiting for you")
    #expect(
        NeedText(Need(id: "secret/x", kind: .remove, secretName: "x")).body
            == #"Removing secret "x" (name chosen by the requester) needs Touch ID."#)
    let owner = NeedText(
        Need(id: "session/1", kind: .leftover, ownerName: "node\u{1B}[2J\" ok", browser: .headlessShell))
    #expect(
        owner.body
            == #"A headless shell browser outlived its owner "node\x1b[2J\" ok" (name chosen by the process). Berth can stop it."#
    )
    #expect(
        NeedText(Need(id: "session/1", kind: .leftover)).body
            == "A browser outlived whatever started it. Berth can stop it.")
    #expect(NeedText(Need(id: "x/1", kind: .unspecified)).title == "Berth needs you")
}

@MainActor
private final class RecordingNotifier: Notifier {
    var log: [String] = []
    func post(id: String, text: NeedText) { log.append("post \(id): \(text.title)") }
    func remove(id: String) { log.append("remove \(id)") }
}

private final class ScriptedNeeds: NeedSource, @unchecked Sendable {
    private let lock = NSLock()
    private var scripts: [[WatchNeedsResponse]]
    private(set) var afters: [Int64] = []

    init(_ scripts: [[WatchNeedsResponse]]) { self.scripts = scripts }

    func watchNeeds(after: Int64) -> AsyncThrowingStream<WatchNeedsResponse, any Error> {
        let messages = lock.withLock {
            afters.append(after)
            return scripts.isEmpty ? [] : scripts.removeFirst()
        }
        return AsyncThrowingStream { continuation in
            for m in messages { continuation.yield(m) }
            continuation.finish(throwing: ClientError.unreachable("gone"))
        }
    }

    var calls: [Int64] { lock.withLock { afters } }
}

/// A sleeper that cancels the watch on its nth call.
private final class Countdown: @unchecked Sendable {
    private let lock = NSLock()
    private var left: Int

    init(_ n: Int) { left = n }

    func tick() throws {
        try lock.withLock {
            left -= 1
            if left <= 0 { throw CancellationError() }
        }
    }
}

@MainActor @Test func theModelPostsOnlyWhileTurnedOnAndResumes() async {
    let source = ScriptedNeeds([
        [.snapshot(NeedsSnapshot(eventID: 4, needs: [leftover], reason: .start))],
        [.change(NeedChange(eventID: 9, event: .opened, need: store))],
    ])
    let notifier = RecordingNotifier()
    let sleeps = Countdown(2)
    let model = NeedsModel(source: source, notifier: notifier, sleep: { _ in try sleeps.tick() })
    await model.run()
    #expect(source.calls == [0, 4])
    #expect(notifier.log.isEmpty)
    #expect(model.link == .reconnecting("berthd is not reachable: gone"))
    model.setPosting(true)
    // In id order: secret/ before session/.
    #expect(
        notifier.log == [
            "post \(store.id): A secret is waiting to be stored", "post \(leftover.id): A browser was left running",
        ])
    model.setPosting(true)
    model.setPosting(false)
    #expect(notifier.log.suffix(2) == ["remove \(store.id)", "remove \(leftover.id)"])
}
