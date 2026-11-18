import Foundation

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

/// How long a watch waits before it reconnects: 1 second, doubling to at most 10, and back to 1
/// after any message.
public enum Backoff {
    public static let first = Duration.seconds(1)
    public static let max = Duration.seconds(10)
}

/// Watches a resumable stream until the calling task is cancelled: each time the stream ends or
/// fails it reports why, waits, and opens it again after the cursor of the last message applied,
/// so nothing is missed or applied twice.
@MainActor
func resumeWatching<Message: Sendable>(
    open: (Int64) -> AsyncThrowingStream<Message, any Error>,
    cursor: () -> Int64,
    apply: (Message) -> Void,
    link: (Link) -> Void,
    sleep: @Sendable (Duration) async throws -> Void
) async {
    var retry = Backoff.first
    while !Task.isCancelled {
        do {
            for try await message in open(cursor()) {
                apply(message)
                link(.watching)
                retry = Backoff.first
            }
            link(.reconnecting("berthd ended the stream"))
        } catch is CancellationError {
            return
        } catch {
            link(.reconnecting(shown(String(describing: error))))
        }
        do {
            try await sleep(retry)
        } catch {
            return
        }
        retry = min(retry * 2, Backoff.max)
    }
}
