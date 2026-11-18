import Foundation
import Testing

@testable import BerthAppCore

@MainActor
private final class FakeLoginItem: LoginItemService {
    var status = LoginItemStatus.notRegistered
    var registers = 0
    var unregisters = 0
    var opens = 0
    var failure: (any Error)?

    func register() throws {
        registers += 1
        if let failure { throw failure }
        status = .requiresApproval
    }

    func unregister() throws {
        unregisters += 1
        if let failure { throw failure }
        status = .notRegistered
    }

    func openSettings() { opens += 1 }
}

private struct FixedStatus: StatusSource {
    var answer: Result<StatusResponse, ClientError>
    func status() async throws -> StatusResponse { try answer.get() }
}

private let bundled = "/Applications/Berth.app/Contents/Helpers/berthd"

@MainActor @Test func theModelNeverRegistersOnItsOwn() async {
    let item = FakeLoginItem()
    let model = LoginItemModel(
        service: item,
        source: FixedStatus(answer: .success(StatusResponse(pid: 7, daemonVersion: "dev", executable: bundled))),
        bundledDaemon: bundled)
    await model.refresh()
    #expect(item.registers == 0 && item.unregisters == 0 && item.opens == 0)
    #expect(model.status == .notRegistered)
    #expect(model.daemon == "the bundle's berthd, \(bundled), pid 7, version dev")
}

@MainActor @Test func buttonsRegisterAndUnregister() {
    let item = FakeLoginItem()
    let model = LoginItemModel(
        service: item, source: FixedStatus(answer: .failure(.unreachable("x"))), bundledDaemon: bundled)
    model.register()
    #expect(item.registers == 1)
    #expect(model.status == .requiresApproval)
    model.openSettings()
    #expect(item.opens == 1)
    model.unregister()
    #expect(item.unregisters == 1)
    #expect(model.status == .notRegistered)
    #expect(model.failure == nil)
}

@MainActor @Test func failuresAndForeignDaemonsAreEscaped() async {
    let item = FakeLoginItem()
    item.failure = ClientError.malformed("no\u{1B}[2J")
    let model = LoginItemModel(
        service: item,
        source: FixedStatus(
            answer: .success(StatusResponse(pid: 9, daemonVersion: "v\u{202E}1", executable: "/tmp/bin/berthd"))),
        bundledDaemon: bundled)
    model.register()
    #expect(model.failure == #""malformed answer from berthd: no\x1b[2J""#)
    #expect(model.status == .notRegistered)
    await model.refresh()
    #expect(model.daemon == "a berthd started by hand, /tmp/bin/berthd, pid 9, version \"v\\u202e1\"")
}

@MainActor @Test func noAnswerSaysWhy() async {
    let model = LoginItemModel(
        service: FakeLoginItem(), source: FixedStatus(answer: .failure(.unreachable("refused\n"))),
        bundledDaemon: bundled)
    await model.refresh()
    #expect(model.daemon == #"no answer: "berthd is not reachable: refused\n""#)
}

/// The Go test wrote this from a StatusResponse that sets every field.
@Test func statusFixtureDecodesEveryField() throws {
    let s = try JSONDecoder().decode(StatusResponse.self, from: fixture("status.json"))
    #expect(s.state == "Serving")
    #expect(s.stateVersion == 9_007_199_254_740_993)
    #expect(s.pid == 4321)
    #expect(s.startedAt == Date(timeIntervalSince1970: 1_763_380_000))
    #expect(s.socketPath == "/Users/u/Library/Application Support/Berth/berthd.sock")
    #expect(s.daemonVersion == "dev")
    #expect(s.executable == "/Applications/Berth.app/Contents/Helpers/berthd")
}
