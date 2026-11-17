import Foundation

/// The sessions the app shows, folded from WatchSessions exactly as berth top folds them: a
/// snapshot replaces everything, a change replaces one session, keeping the facts it already had
/// when the change carries only the id and state.
public struct SessionStore: Sendable, Equatable {
    /// berth top keeps as many stopped sessions.
    public static let stoppedMax = 200

    public private(set) var sessions: [String: Session] = [:]
    /// The event id of the last message applied; a reconnect resumes after it.
    public private(set) var cursor: Int64 = 0

    public init() {}

    public mutating func apply(_ message: WatchSessionsResponse) {
        switch message {
        case .snapshot(let snapshot):
            sessions = Dictionary(snapshot.sessions.map { ($0.id, $0) }, uniquingKeysWith: { _, last in last })
            cursor = snapshot.eventID
        case .change(let change):
            var session = change.session
            if session.browser == nil, var known = sessions[session.id] {
                known.state = session.state
                known.stateSince = session.stateSince
                session = known
            }
            sessions[session.id] = session
            cursor = change.eventID
            if session.state == .stopped { trimStopped() }
        case .unknown:
            break
        }
    }

    private mutating func trimStopped() {
        let stopped = sessions.values.filter { $0.state == .stopped }
        guard stopped.count > Self.stoppedMax else { return }
        let oldest = stopped.sorted {
            ($0.stateSince ?? .distantPast, $0.id) < ($1.stateSince ?? .distantPast, $1.id)
        }
        for session in oldest.prefix(stopped.count - Self.stoppedMax) { sessions[session.id] = nil }
    }

    /// Live sessions, newest browser first.
    public var live: [Session] { ordered(sessions.values.filter { $0.state != .stopped }) }

    /// Stopped sessions, most recently stopped first.
    public var stopped: [Session] {
        sessions.values.filter { $0.state == .stopped }.sorted {
            ($0.stateSince ?? .distantPast, $0.id) > ($1.stateSince ?? .distantPast, $1.id)
        }
    }

    private func ordered(_ list: [Session]) -> [Session] {
        list.sorted {
            ($0.browser?.startedAt ?? .distantPast, $0.id) > ($1.browser?.startedAt ?? .distantPast, $1.id)
        }
    }

    public var counts: Counts {
        var counts = Counts()
        for session in sessions.values where session.state != .stopped {
            counts.live += 1
            switch session.state {
            case .active: counts.active += 1
            case .idle: counts.idle += 1
            case .leftover: counts.leftover += 1
            case .discovered, .stopping, .unspecified, .stopped: counts.other += 1
            }
        }
        return counts
    }

    public struct Counts: Equatable, Sendable {
        public var live = 0
        public var active = 0
        public var idle = 0
        public var leftover = 0
        public var other = 0
    }
}

extension Session {
    /// Who started the browser, as the list shows it, escaped like the CLI.
    public var ownerText: String {
        guard let owner else { return "unknown" }
        return owner.name.isEmpty ? owner.kind.label : "\(owner.kind.label) \(shown(owner.name))"
    }

    public var profileText: String { userDataDir.isEmpty ? "" : shown(userDataDir) }

    /// The browser for the list: its kind, or another user's, or unreadable arguments.
    public var browserText: String {
        if otherUser { return "\(kind.label), other user" }
        return kind.label
    }

    public var headlessText: String {
        otherUser || argsUnreadable ? "unknown" : (headless ? "yes" : "no")
    }

    public var driverText: String { argsUnreadable ? "unknown" : driver.label }
}
