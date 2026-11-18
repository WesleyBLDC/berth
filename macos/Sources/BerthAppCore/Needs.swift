import Foundation

/// What a notification shows for a need. Every name a process or an agent chose is quoted and
/// labelled with who chose it, never taken as Berth's own words.
public struct NeedText: Equatable, Sendable {
    public var title: String
    public var body: String

    public init(_ need: Need) {
        let secret = "secret \(quoted(need.secretName)) (name chosen by the requester)"
        switch need.kind {
        case .store:
            title = "A secret is waiting to be stored"
            body = "The store dialog for \(secret) is open."
        case .rotate:
            title = "A rotate is waiting for you"
            body = "The rotate dialog for \(secret) is open."
        case .remove:
            title = "A remove is waiting for Touch ID"
            body = "Removing \(secret) needs Touch ID."
        case .grant:
            title = "A grant is waiting for Touch ID"
            body = "Granting \(secret) needs Touch ID."
        case .leftover:
            title = "A browser was left running"
            let browser = need.browser == .unspecified ? "A browser" : "A \(need.browser.label) browser"
            body =
                need.ownerName.isEmpty
                ? "\(browser) outlived whatever started it. Berth can stop it."
                : "\(browser) outlived its owner \(quoted(need.ownerName)) (name chosen by the process). Berth can stop it."
        case .unspecified:
            title = "Berth needs you"
            body = "Something waits on you; open Berth to see it."
        }
    }
}

/// What the app must do with notifications after a message.
public enum NeedAction: Equatable, Sendable {
    case post(Need)
    case remove(String)
}

/// The open needs, folded from WatchNeeds. A need is posted once while it stays open, however
/// often a snapshot or a repeated change shows it again, and its notification is removed when it
/// resolves or a snapshot no longer holds it.
public struct NeedsStore: Sendable, Equatable {
    public private(set) var open: [String: Need] = [:]
    /// The event id of the last message applied; a reconnect resumes after it.
    public private(set) var cursor: Int64 = 0
    private var posted: Set<String> = []

    public init() {}

    /// Applies a message and returns what to post and remove; with posting off it only follows
    /// the needs.
    public mutating func apply(_ message: WatchNeedsResponse, posting: Bool) -> [NeedAction] {
        var actions: [NeedAction] = []
        switch message {
        case .snapshot(let snapshot):
            let next = Dictionary(snapshot.needs.map { ($0.id, $0) }, uniquingKeysWith: { _, last in last })
            for id in posted.subtracting(next.keys).sorted() {
                posted.remove(id)
                actions.append(.remove(id))
            }
            open = next
            cursor = snapshot.eventID
        case .change(let change):
            cursor = change.eventID
            switch change.event {
            case .opened:
                open[change.need.id] = change.need
            case .resolved:
                open[change.need.id] = nil
                if posted.remove(change.need.id) != nil { actions.append(.remove(change.need.id)) }
            case .unspecified:
                break
            }
        case .unknown:
            break
        }
        return actions + (posting ? unposted() : [])
    }

    /// Needs open now that have not been posted, oldest id first, marking them posted.
    public mutating func unposted() -> [NeedAction] {
        let fresh = open.keys.filter { !posted.contains($0) }.sorted()
        posted.formUnion(fresh)
        return fresh.compactMap { open[$0] }.map(NeedAction.post)
    }

    /// Forgets what was posted, when the person turns notifications off.
    public mutating func forgetPosted() -> [NeedAction] {
        defer { posted = [] }
        return posted.sorted().map(NeedAction.remove)
    }
}
