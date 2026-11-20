import BerthAppCore
import Foundation
import SwiftUI
import Testing

@testable import BerthApp

/// The app's views drawn from the fixtures the Go tests write, in light and dark, against the
/// images in Snapshots/. Nothing is shown on screen and nothing asks for a permission.
@MainActor @Suite(.serialized)
struct ViewSnapshotTests {
    @Test(arguments: Look.allCases)
    func sessionList(look: Look) async throws {
        let model = SessionsModel(source: FixedSessions(try sessionMessages()))
        let watch = try await watching(model.run, until: { model.link == .watching })
        defer { watch.cancel() }
        try Snapshot.check(
            SessionsWindow(model: model), named: "sessions", look: look, size: CGSize(width: 980, height: 150))
    }

    @Test(arguments: Look.allCases)
    func settings(look: Look) async throws {
        let needs = NeedsModel(source: FixedNeeds([]), notifier: NoNotifier())
        let loginItem = LoginItemModel(
            service: FixedLoginItem(), source: FixedStatus(), bundledDaemon: try status().executable)
        await loginItem.refresh()
        try Snapshot.check(
            SettingsSection(loginItem: loginItem, notifications: NotificationSetting(needs: needs)), named: "settings",
            look: look, size: CGSize(width: 640, height: 360))
    }

    /// The menu bar menu with the fixture's sessions and its open need: the need reaches the
    /// window side of the app only as this line, and otherwise as a system notification.
    @Test(arguments: Look.allCases)
    func menuWithNeeds(look: Look) async throws {
        let model = SessionsModel(source: FixedSessions(try sessionMessages()))
        let needs = NeedsModel(source: FixedNeeds(try needMessages()), notifier: NoNotifier())
        let watch = try await watching(model.run, until: { model.link == .watching })
        defer { watch.cancel() }
        let needsWatch = try await watching(needs.run, until: { needs.link == .watching })
        defer { needsWatch.cancel() }
        try Snapshot.check(
            VStack(alignment: .leading, spacing: 6) { MenuContent(model: model, needs: needs) }.padding(12),
            named: "menu", look: look, size: CGSize(width: 300, height: 170))
    }

    @Test func darkImagesAreLightTextOnDark() async throws {
        let model = SessionsModel(source: FixedSessions(try sessionMessages()))
        let watch = try await watching(model.run, until: { model.link == .watching })
        defer { watch.cancel() }
        let size = CGSize(width: 980, height: 150)
        let dark = try Snapshot.render(SessionsWindow(model: model), look: .dark, size: size)
        let light = try Snapshot.render(SessionsWindow(model: model), look: .light, size: size)
        #expect(dark.luminance() < 0.3, "the dark list is not on a dark background")
        #expect(light.luminance() > 0.7, "the light list is not on a light background")
        #expect(dark.brightestShare(above: 0.75) > 0.005, "the dark list has too little light text")
    }
}

/// Fixture times are moved so the fixture's browser started 3 hours, 30 minutes and 30 seconds
/// before the moment the view reads the clock: the relative "Started" text is then the same on
/// every run, with 30 seconds of slack on either side.
let fixtureNow = Date(timeIntervalSince1970: 1_763_380_000 + 3 * 3600 + 30 * 60 + 30)

/// The Go written snapshot, its one session moved to the fixed now, beside two variants of it so
/// the list shows more than one row and the menu more than one state.
func sessionMessages() throws -> [WatchSessionsResponse] {
    let messages = try JSONDecoder().decode([WatchSessionsResponse].self, from: sharedFixture("sessions.json"))
    guard case .snapshot(var snapshot) = messages.first, var session = snapshot.sessions.first else {
        throw SnapshotError.render("sessions.json holds no snapshot")
    }
    let shift = Date().timeIntervalSince(fixtureNow)
    session.browser?.startedAt?.addTimeInterval(shift)
    session.owner?.process?.startedAt?.addTimeInterval(shift)
    session.stateSince?.addTimeInterval(shift)

    var idle = session
    idle.id = "5151-1763370000000000"
    idle.state = .idle
    idle.kind = .chrome
    idle.driver = .none
    idle.headless = false
    idle.otherUser = false
    idle.argsUnreadable = false
    idle.userDataDir = "/Users/person/Library/Application Support/Google/Chrome"
    idle.owner?.kind = .person
    idle.owner?.name = ""
    idle.browser?.startedAt?.addTimeInterval(-2 * 86400)

    var leftover = session
    leftover.id = "6262-1763379000000000"
    leftover.state = .leftover
    leftover.kind = .headlessShell
    leftover.driver = .puppeteer
    leftover.otherUser = false
    leftover.argsUnreadable = false
    leftover.owner?.kind = .process
    leftover.owner?.name = "node\u{202E}"
    leftover.browser?.startedAt?.addTimeInterval(-1500)

    snapshot.sessions = [session, idle, leftover]
    return [.snapshot(snapshot)]
}

func needMessages() throws -> [WatchNeedsResponse] {
    try JSONDecoder().decode([WatchNeedsResponse].self, from: sharedFixture("needs.json"))
}

func status() throws -> StatusResponse {
    try JSONDecoder().decode(StatusResponse.self, from: sharedFixture("status.json"))
}

/// The fixtures BerthAppCoreTests reads, which Go tests write.
func sharedFixture(_ name: String) throws -> Data {
    try Data(
        contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("BerthAppCoreTests/Fixtures").appendingPathComponent(name))
}

/// Starts a model's watch and returns once it has applied the fixture; the stream stays open, so
/// the link reads watching until the caller cancels the returned task.
@MainActor
func watching(_ run: @escaping @MainActor () async -> Void, until done: @MainActor () -> Bool) async throws -> Task<
    Void, Never
> {
    let task = Task { await run() }
    for _ in 0..<1000 where !done() { await Task.yield() }
    guard done() else {
        task.cancel()
        throw SnapshotError.render("the model never applied the fixture")
    }
    return task
}

/// Streams the given messages, then stays open like a quiet berthd: the stream holds its own
/// continuation until it is cancelled, since dropping the last one would end it.
func open<Message: Sendable>(_ messages: [Message]) -> AsyncThrowingStream<Message, any Error> {
    AsyncThrowingStream { continuation in
        for message in messages { continuation.yield(message) }
        continuation.onTermination = { _ in _ = continuation }
    }
}

struct FixedSessions: SessionSource {
    let messages: [WatchSessionsResponse]
    init(_ messages: [WatchSessionsResponse]) { self.messages = messages }

    func watch(after: Int64) -> AsyncThrowingStream<WatchSessionsResponse, any Error> {
        open(messages)
    }
}

struct FixedNeeds: NeedSource {
    let messages: [WatchNeedsResponse]
    init(_ messages: [WatchNeedsResponse]) { self.messages = messages }

    func watchNeeds(after: Int64) -> AsyncThrowingStream<WatchNeedsResponse, any Error> {
        open(messages)
    }
}

struct FixedStatus: StatusSource {
    func status() async throws -> StatusResponse { try BerthAppSnapshotTests.status() }
}

@MainActor
struct FixedLoginItem: LoginItemService {
    var status: LoginItemStatus { .enabled }
    func register() throws {}
    func unregister() throws {}
    func openSettings() {}
}

@MainActor
struct NoNotifier: Notifier {
    func post(id: String, text: NeedText) {}
    func remove(id: String) {}
}

extension Pixels {
    /// The share of pixels brighter than the given relative luminance.
    func brightestShare(above level: Double) -> Double {
        var count = 0
        for pixel in 0..<(width * height) {
            let at = pixel * 4
            let value =
                (0.2126 * Double(bytes[at]) + 0.7152 * Double(bytes[at + 1]) + 0.0722 * Double(bytes[at + 2])) / 255
            if value > level { count += 1 }
        }
        return Double(count) / Double(max(width * height, 1))
    }
}
