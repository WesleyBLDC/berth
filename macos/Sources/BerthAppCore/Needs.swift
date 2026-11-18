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
/// often a snapshot or a repeated change shows it again; it is posted again, replacing its
/// notification by id, when its facts change (a secret moving from one dialog to another), and
/// its notification is removed when it resolves or a snapshot no longer holds it.
public struct NeedsStore: Sendable, Equatable {
    public private(set) var open: [String: Need] = [:]
    /// The event id of the last message applied; a reconnect resumes after it.
    public private(set) var cursor: Int64 = 0
    /// What each posted notification shows, by need id.
    private var posted: [String: Need] = [:]

    public init() {}

    /// Applies a message and returns what to post and remove; with posting off it only follows
    /// the needs.
    public mutating func apply(_ message: WatchNeedsResponse, posting: Bool) -> [NeedAction] {
        var actions: [NeedAction] = []
        switch message {
        case .snapshot(let snapshot):
            let next = Dictionary(snapshot.needs.map { ($0.id, $0) }, uniquingKeysWith: { _, last in last })
            for id in posted.keys.filter({ next[$0] == nil }).sorted() {
                posted[id] = nil
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
                if posted.removeValue(forKey: change.need.id) != nil { actions.append(.remove(change.need.id)) }
            case .unspecified:
                break
            }
        case .cursor(let eventID):
            cursor = eventID
        case .unknown:
            break
        }
        return actions + (posting ? unposted() : [])
    }

    /// Needs open now that are not posted as they are, in id order, marking them posted.
    public mutating func unposted() -> [NeedAction] {
        let fresh = open.values.filter { posted[$0.id] != $0 }.sorted { $0.id < $1.id }
        for need in fresh { posted[need.id] = need }
        return fresh.map(NeedAction.post)
    }

    /// Forgets what was posted, when the person turns notifications off.
    public mutating func forgetPosted() -> [NeedAction] {
        defer { posted = [:] }
        return posted.keys.sorted().map(NeedAction.remove)
    }
}
