import Foundation
import Testing

@testable import BerthAppCore

private func session(_ id: String, _ state: SessionState, started: TimeInterval = 0, owner: String = "claude")
    -> Session
{
    var s = Session(id: id, state: state)
    var browser = SessionProcess()
    browser.pid = 1
    browser.startedAt = Date(timeIntervalSince1970: started)
    s.browser = browser
    var o = SessionOwner()
    o.kind = .agent
    o.name = owner
    s.owner = o
    s.stateSince = Date(timeIntervalSince1970: started)
    return s
}

@Test func aSnapshotReplacesAndAChangeUpserts() {
    var store = SessionStore()
    store.apply(.snapshot(SessionsSnapshot(eventID: 5, sessions: [session("a", .idle)], reason: .start)))
    store.apply(.change(SessionChange(eventID: 8, event: "Seen", session: session("b", .discovered, started: 10))))
    #expect(store.cursor == 8)
    #expect(store.live.map(\.id) == ["b", "a"])
    store.apply(.snapshot(SessionsSnapshot(eventID: 20, sessions: [session("c", .active)], reason: .behind)))
    #expect(store.live.map(\.id) == ["c"])
    #expect(store.cursor == 20)
    store.apply(.unknown)
    #expect(store.cursor == 20)
}

@Test func aStateOnlyChangeKeepsTheFacts() {
    var store = SessionStore()
    store.apply(
        .snapshot(SessionsSnapshot(eventID: 1, sessions: [session("a", .active, owner: "codex")], reason: .start)))
    var bare = Session(id: "a", state: .stopped)
    bare.stateSince = Date(timeIntervalSince1970: 99)
    store.apply(.change(SessionChange(eventID: 2, event: "Exited", session: bare)))
    let a = store.stopped.first
    #expect(a?.state == .stopped)
    #expect(a?.owner?.name == "codex")
    #expect(a?.stateSince == Date(timeIntervalSince1970: 99))
    #expect(store.live.isEmpty)
    store.apply(.change(SessionChange(eventID: 3, event: "Exited", session: Session(id: "z", state: .stopped))))
    #expect(store.sessions["z"]?.owner == nil)
}

@Test func stoppedSessionsAreTrimmedOldestFirst() {
    var store = SessionStore()
    for i in 0..<(SessionStore.stoppedMax + 5) {
        store.apply(
            .change(
                SessionChange(
                    eventID: Int64(i), event: "Exited", session: session("s\(i)", .stopped, started: Double(i)))))
    }
    #expect(store.stopped.count == SessionStore.stoppedMax)
    #expect(store.sessions["s0"] == nil && store.sessions["s4"] == nil)
    #expect(store.stopped.first?.id == "s\(SessionStore.stoppedMax + 4)")
}

@Test func countsGoByState() {
    var store = SessionStore()
    store.apply(
        .snapshot(
            SessionsSnapshot(
                eventID: 1,
                sessions: [
                    session("a", .active), session("b", .idle), session("c", .leftover), session("d", .leftover),
                    session("e", .discovered), session("f", .stopped),
                ], reason: .start)))
    #expect(store.counts == SessionStore.Counts(live: 5, active: 1, idle: 1, leftover: 2, other: 1))
}

@Test func listTextIsEscaped() {
    var s = session("a", .active, owner: "evil\u{1B}]0;title\u{07}")
    s.userDataDir = "/tmp/\u{202E}gpj.exe"
    #expect(s.ownerText == #"agent "evil\x1b]0;title\a""#)
    #expect(s.profileText == "\"/tmp/\\" + "u202egpj.exe\"")
    s.argsUnreadable = true
    #expect(s.headlessText == "unknown" && s.driverText == "unknown")
}
