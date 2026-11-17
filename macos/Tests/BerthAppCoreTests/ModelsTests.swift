import Foundation
import Testing

@testable import BerthAppCore

/// The Go test wrote this from a WatchSessionsResponse that sets every Session field.
@Test func fixtureDecodesEveryField() throws {
    let messages = try JSONDecoder().decode([WatchSessionsResponse].self, from: fixture("sessions.json"))
    guard messages.count == 2, case .snapshot(let snapshot) = messages[0], case .change(let change) = messages[1]
    else {
        Issue.record("unexpected messages \(messages)")
        return
    }
    #expect(snapshot.eventID == 9_007_199_254_740_993)
    #expect(snapshot.reason == .start)
    let s = try #require(snapshot.sessions.first)
    #expect(s.id == "4242-1763380000123456")
    #expect(s.state == .active)
    #expect(s.kind == .chromeForTesting)
    #expect(s.driver == .playwright)
    #expect(s.headless)
    #expect(s.cdp == .port)
    #expect(s.cdpPort == 9222)
    #expect(s.userDataDir == "/tmp/berth profile")
    #expect(s.owner?.kind == .agent)
    #expect(s.owner?.name == "claude\u{1B}[2J")
    #expect(s.owner?.process?.pid == 4000)
    #expect(s.owner?.process?.startedAt == Date(timeIntervalSince1970: 1_763_379_990))
    #expect(s.owner?.process?.executable == "/usr/local/bin/claude")
    #expect(s.owner?.process?.role == .ancestor)
    #expect(s.owner?.process?.executableChanged == true)
    #expect(s.browser?.executableChanged == true)
    #expect(s.browser?.pid == 4242)
    #expect(s.browser?.startedAt == Date(timeIntervalSince1970: 1_763_380_000.123456))
    #expect(s.browser?.executable == "/opt/chrome/Chrome for Testing")
    #expect(s.browser?.role == .browser)
    #expect(s.processCount == 7)
    #expect(s.otherUser)
    #expect(s.uid == 501)
    let since = try #require(s.stateSince).timeIntervalSince1970
    #expect(abs(since - 1_763_380_005.123456789) < 1e-6)
    #expect(s.argsUnreadable)
    #expect(change.eventID == 9_007_199_254_740_995)
    #expect(change.event == "Exited")
    #expect(change.session.state == .stopped)
    #expect(change.session.browser == nil)
}

@Test func unknownNamesAndOneofCasesDoNotFail() throws {
    let json = #"[{"change":{"eventId":"7","session":{"id":"1-2","state":"SESSION_STATE_NEW","kind":3}}},{"other":{}}]"#
    let messages = try JSONDecoder().decode([WatchSessionsResponse].self, from: Data(json.utf8))
    guard case .change(let change) = messages[0] else {
        Issue.record("not a change")
        return
    }
    #expect(change.session.state == .unspecified)
    #expect(change.session.state.label == "unknown")
    #expect(change.session.kind == .unspecified)
    #expect(change.eventID == 7)
    #expect(messages[1] == .unknown)
}

@Test func timestampsTakeEveryPrecision() {
    #expect(parseTimestamp("2026-11-17T10:00:00Z") == Date(timeIntervalSince1970: 1_794_909_600))
    #expect(parseTimestamp("2026-11-17T10:00:00.5Z") == Date(timeIntervalSince1970: 1_794_909_600.5))
    #expect(parseTimestamp("2026-11-17T10:00:00.250Z") == Date(timeIntervalSince1970: 1_794_909_600.25))
    #expect(parseTimestamp("2026-11-17T10:00:00+01:00") == nil)
    #expect(parseTimestamp("2026-11-17T10:00:00.1234567890Z") == nil)
    #expect(parseTimestamp("2026-11-17T10:00:00.Z") == nil)
}

@Test func requestsUseProtoJSONNames() throws {
    let watch = try JSONEncoder().encode(WatchSessionsRequest(afterEventId: ProtoInt64(9_007_199_254_740_993)))
    #expect(String(decoding: watch, as: UTF8.self) == #"{"afterEventId":"9007199254740993"}"#)
}
