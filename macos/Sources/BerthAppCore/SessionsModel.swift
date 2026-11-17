import Foundation
import Observation

/// What the app shows about its link to berthd.
public enum Link: Equatable, Sendable {
    case connecting
    case watching
    /// The stream ended or failed; the text is escaped and ready to show.
    case reconnecting(String)

    public var text: String {
        switch self {
        case .connecting: "connecting to berthd"
        case .watching: "watching"
        case .reconnecting(let why): "reconnecting: \(why)"
        }
    }
}

/// The session list's model: it watches sessions from berthd for as long as run() runs, and
/// when a stream ends or fails it resumes after the last event id it applied, so nothing is
/// missed or applied twice.
@MainActor @Observable
public final class SessionsModel {
    public private(set) var store = SessionStore()
    public private(set) var link = Link.connecting

    public static let firstRetry = Duration.seconds(1)
    public static let maxRetry = Duration.seconds(10)

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
        var retry = Self.firstRetry
        while !Task.isCancelled {
            do {
                for try await message in source.watch(after: store.cursor) {
                    store.apply(message)
                    link = .watching
                    retry = Self.firstRetry
                }
                link = .reconnecting("berthd ended the stream")
            } catch is CancellationError {
                return
            } catch {
                link = .reconnecting(shown(String(describing: error)))
            }
            do {
                try await sleep(retry)
            } catch {
                return
            }
            retry = min(retry * 2, Self.maxRetry)
        }
    }
}
