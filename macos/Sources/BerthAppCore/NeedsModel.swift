import Foundation
import Observation

/// Where the app watches needs from; the real one is DaemonClient, tests use a fake.
public protocol NeedSource: Sendable {
    /// Handshakes, then streams WatchNeeds after the given event id (0 starts with a snapshot).
    func watchNeeds(after: Int64) -> AsyncThrowingStream<WatchNeedsResponse, any Error>
}

/// Posts and removes notifications; the real one wraps UNUserNotificationCenter in the app.
@MainActor
public protocol Notifier {
    func post(id: String, text: NeedText)
    func remove(id: String)
}

/// The needs model: it watches what waits on the person for as long as run() runs, and posts a
/// notification per need only while the person has turned notifications on.
@MainActor @Observable
public final class NeedsModel {
    public private(set) var store = NeedsStore()
    public private(set) var link = Link.connecting
    public private(set) var posting = false

    @ObservationIgnored private let source: any NeedSource
    @ObservationIgnored private let notifier: any Notifier
    @ObservationIgnored private let sleep: @Sendable (Duration) async throws -> Void

    public init(
        source: any NeedSource, notifier: any Notifier,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.source = source
        self.notifier = notifier
        self.sleep = sleep
    }

    /// Runs until the calling task is cancelled.
    public func run() async {
        await resumeWatching(
            open: source.watchNeeds(after:), cursor: { store.cursor },
            apply: { perform(store.apply($0, posting: posting)) }, link: { link = $0 }, sleep: sleep)
    }

    /// Turning posting on posts every open need not posted yet; turning it off removes them.
    public func setPosting(_ on: Bool) {
        guard on != posting else { return }
        posting = on
        perform(on ? store.unposted() : store.forgetPosted())
    }

    private func perform(_ actions: [NeedAction]) {
        for action in actions {
            switch action {
            case .post(let need): notifier.post(id: need.id, text: NeedText(need))
            case .remove(let id): notifier.remove(id: id)
            }
        }
    }
}
