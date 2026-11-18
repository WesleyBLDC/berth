import Foundation
import Observation

/// The session list's model: it watches sessions from berthd for as long as run() runs.
@MainActor @Observable
public final class SessionsModel {
    public private(set) var store = SessionStore()
    public private(set) var link = Link.connecting

    @ObservationIgnored private let source: any SessionSource
    @ObservationIgnored private let sleep: @Sendable (Duration) async throws -> Void

    public init(
        source: any SessionSource,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.source = source
        self.sleep = sleep
    }

    /// Runs until the calling task is cancelled.
    public func run() async {
        await resumeWatching(
            open: source.watch(after:), cursor: { store.cursor }, apply: { store.apply($0) }, link: { link = $0 },
            sleep: sleep)
    }
}
