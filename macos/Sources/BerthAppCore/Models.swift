import Foundation

// Hand written models for the berth.v1 messages the app uses, in the proto JSON mapping:
// lowerCamelCase names, enums by name, 64 bit integers as strings, RFC 3339 timestamps, and
// default values omitted. Fixtures/sessions.json is written by a Go test from the real schema,
// so a renamed or removed field fails the tests on both sides (ADR 0019).

/// A proto enum read by name. A name this build does not know (a newer berthd) reads as the
/// unspecified value, which the app shows as unknown, never a decoding failure.
public protocol ProtoEnum: RawRepresentable<String>, Codable, Sendable, Hashable, CaseIterable {
    static var unspecified: Self { get }
}

extension ProtoEnum {
    public init(from decoder: any Decoder) throws {
        let container = try decoder.singleValueContainer()
        self = (try? container.decode(String.self)).flatMap(Self.init(rawValue:)) ?? .unspecified
    }
}

public enum SessionState: String, ProtoEnum {
    case unspecified = "SESSION_STATE_UNSPECIFIED"
    case discovered = "SESSION_STATE_DISCOVERED"
    case active = "SESSION_STATE_ACTIVE"
    case idle = "SESSION_STATE_IDLE"
    case leftover = "SESSION_STATE_LEFTOVER"
    case stopping = "SESSION_STATE_STOPPING"
    case stopped = "SESSION_STATE_STOPPED"

    public var label: String {
        self == .unspecified ? "unknown" : String(rawValue.dropFirst("SESSION_STATE_".count)).lowercased()
    }
}

public enum BrowserKind: String, ProtoEnum {
    case unspecified = "BROWSER_KIND_UNSPECIFIED"
    case chrome = "BROWSER_KIND_CHROME"
    case chromeForTesting = "BROWSER_KIND_CHROME_FOR_TESTING"
    case chromium = "BROWSER_KIND_CHROMIUM"
    case edge = "BROWSER_KIND_EDGE"
    case brave = "BROWSER_KIND_BRAVE"
    case headlessShell = "BROWSER_KIND_HEADLESS_SHELL"

    public var label: String {
        switch self {
        case .unspecified: "unknown"
        case .chrome: "Chrome"
        case .chromeForTesting: "Chrome for Testing"
        case .chromium: "Chromium"
        case .edge: "Edge"
        case .brave: "Brave"
        case .headlessShell: "headless shell"
        }
    }
}

public enum Driver: String, ProtoEnum {
    case unspecified = "DRIVER_UNSPECIFIED"
    case none = "DRIVER_NONE"
    case playwright = "DRIVER_PLAYWRIGHT"
    case puppeteer = "DRIVER_PUPPETEER"
    case automation = "DRIVER_AUTOMATION"

    public var label: String {
        switch self {
        case .unspecified: "unknown"
        case .none: "none"
        case .playwright: "Playwright"
        case .puppeteer: "Puppeteer"
        case .automation: "automation"
        }
    }
}

public enum CdpMode: String, ProtoEnum {
    case unspecified = "CDP_MODE_UNSPECIFIED"
    case none = "CDP_MODE_NONE"
    case pipe = "CDP_MODE_PIPE"
    case port = "CDP_MODE_PORT"
}

public enum OwnerKind: String, ProtoEnum {
    case unspecified = "OWNER_KIND_UNSPECIFIED"
    case agent = "OWNER_KIND_AGENT"
    case terminal = "OWNER_KIND_TERMINAL"
    case app = "OWNER_KIND_APP"
    case person = "OWNER_KIND_PERSON"
    case none = "OWNER_KIND_NONE"
    case process = "OWNER_KIND_PROCESS"

    public var label: String {
        self == .unspecified ? "unknown" : String(rawValue.dropFirst("OWNER_KIND_".count)).lowercased()
    }
}

public enum ProcessRole: String, ProtoEnum {
    case unspecified = "PROCESS_ROLE_UNSPECIFIED"
    case browser = "PROCESS_ROLE_BROWSER"
    case helper = "PROCESS_ROLE_HELPER"
    case ancestor = "PROCESS_ROLE_ANCESTOR"
}

public enum SnapshotReason: String, ProtoEnum {
    case unspecified = "SNAPSHOT_REASON_UNSPECIFIED"
    case start = "SNAPSHOT_REASON_START"
    case behind = "SNAPSHOT_REASON_BEHIND"
    case unknownCursor = "SNAPSHOT_REASON_UNKNOWN_CURSOR"
}

/// An int64 or uint64 field: proto JSON writes them as strings, and accepts numbers too.
struct ProtoInt64: Codable {
    var value: Int64

    init(_ value: Int64) { self.value = value }

    init(from decoder: any Decoder) throws {
        let container = try decoder.singleValueContainer()
        if let text = try? container.decode(String.self), let value = Int64(text) {
            self.value = value
        } else {
            value = try container.decode(Int64.self)
        }
    }

    func encode(to encoder: any Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(String(value))
    }
}

/// google.protobuf.Timestamp in proto JSON: RFC 3339 in UTC with 0, 3, 6 or 9 fractional digits.
struct ProtoTimestamp: Decodable {
    var date: Date

    init(from decoder: any Decoder) throws {
        let container = try decoder.singleValueContainer()
        let text = try container.decode(String.self)
        guard let date = parseTimestamp(text) else {
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "not an RFC 3339 timestamp")
        }
        self.date = date
    }
}

func parseTimestamp(_ text: String) -> Date? {
    guard text.hasSuffix("Z") else { return nil }
    var base = String(text.dropLast())
    var fraction = 0.0
    if let dot = base.firstIndex(of: ".") {
        let digits = base[base.index(after: dot)...]
        guard (1...9).contains(digits.count), digits.allSatisfy(\.isASCII), let value = Double("0." + digits) else {
            return nil
        }
        fraction = value
        base = String(base[..<dot])
    }
    guard let whole = try? Date(base + "Z", strategy: .iso8601) else { return nil }
    return whole.addingTimeInterval(fraction)
}

extension KeyedDecodingContainer {
    fileprivate func value<T: Decodable>(_ key: Key, _ fallback: T) throws -> T {
        try decodeIfPresent(T.self, forKey: key) ?? fallback
    }

    fileprivate func date(_ key: Key) throws -> Date? {
        try decodeIfPresent(ProtoTimestamp.self, forKey: key)?.date
    }

    fileprivate func int64(_ key: Key) throws -> Int64 {
        try decodeIfPresent(ProtoInt64.self, forKey: key)?.value ?? 0
    }
}

public struct SessionProcess: Decodable, Sendable, Equatable {
    public var pid: Int32 = 0
    public var startedAt: Date?
    public var executable = ""
    public var role = ProcessRole.unspecified
    /// The kernel no longer gives a path for the process, so `executable` is the last one seen.
    public var executableChanged = false

    enum CodingKeys: String, CodingKey { case pid, startedAt, executable, role, executableChanged }

    public init() {}

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        pid = try c.value(.pid, 0)
        startedAt = try c.date(.startedAt)
        executable = try c.value(.executable, "")
        role = try c.value(.role, .unspecified)
        executableChanged = try c.value(.executableChanged, false)
    }
}

public struct SessionOwner: Decodable, Sendable, Equatable {
    public var kind = OwnerKind.unspecified
    public var name = ""
    public var process: SessionProcess?

    enum CodingKeys: String, CodingKey { case kind, name, process }

    public init() {}

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        kind = try c.value(.kind, .unspecified)
        name = try c.value(.name, "")
        process = try c.decodeIfPresent(SessionProcess.self, forKey: .process)
    }
}

public struct Session: Decodable, Sendable, Equatable, Identifiable {
    public var id = ""
    public var state = SessionState.unspecified
    public var kind = BrowserKind.unspecified
    public var driver = Driver.unspecified
    public var headless = false
    public var cdp = CdpMode.unspecified
    public var cdpPort: UInt32 = 0
    public var userDataDir = ""
    public var owner: SessionOwner?
    public var browser: SessionProcess?
    public var processCount: UInt32 = 0
    public var otherUser = false
    public var uid: UInt32 = 0
    public var stateSince: Date?
    public var argsUnreadable = false

    enum CodingKeys: String, CodingKey {
        case id, state, kind, driver, headless, cdp, cdpPort, userDataDir, owner, browser, processCount, otherUser,
            uid, stateSince, argsUnreadable
    }

    public init(id: String, state: SessionState) {
        self.id = id
        self.state = state
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.value(.id, "")
        state = try c.value(.state, .unspecified)
        kind = try c.value(.kind, .unspecified)
        driver = try c.value(.driver, .unspecified)
        headless = try c.value(.headless, false)
        cdp = try c.value(.cdp, .unspecified)
        cdpPort = try c.value(.cdpPort, 0)
        userDataDir = try c.value(.userDataDir, "")
        owner = try c.decodeIfPresent(SessionOwner.self, forKey: .owner)
        browser = try c.decodeIfPresent(SessionProcess.self, forKey: .browser)
        processCount = try c.value(.processCount, 0)
        otherUser = try c.value(.otherUser, false)
        uid = try c.value(.uid, 0)
        stateSince = try c.date(.stateSince)
        argsUnreadable = try c.value(.argsUnreadable, false)
    }
}

public struct SessionsSnapshot: Decodable, Sendable, Equatable {
    public var eventID: Int64 = 0
    public var sessions: [Session] = []
    public var reason = SnapshotReason.unspecified

    enum CodingKeys: String, CodingKey {
        case eventID = "eventId"
        case sessions, reason
    }

    public init(eventID: Int64, sessions: [Session], reason: SnapshotReason) {
        self.eventID = eventID
        self.sessions = sessions
        self.reason = reason
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        eventID = try c.int64(.eventID)
        sessions = try c.value(.sessions, [])
        reason = try c.value(.reason, .unspecified)
    }
}

public struct SessionChange: Decodable, Sendable, Equatable {
    public var eventID: Int64 = 0
    public var event = ""
    public var session = Session(id: "", state: .unspecified)

    enum CodingKeys: String, CodingKey {
        case eventID = "eventId"
        case event, session
    }

    public init(eventID: Int64, event: String, session: Session) {
        self.eventID = eventID
        self.event = event
        self.session = session
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        eventID = try c.int64(.eventID)
        event = try c.value(.event, "")
        session = try c.value(.session, Session(id: "", state: .unspecified))
    }
}

/// WatchSessionsResponse: its oneof is a snapshot or a change. A message holding neither (a
/// newer berthd's new case) decodes as .unknown and is skipped.
public enum WatchSessionsResponse: Decodable, Sendable, Equatable {
    case snapshot(SessionsSnapshot)
    case change(SessionChange)
    case unknown

    enum CodingKeys: String, CodingKey { case snapshot, change }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        if let snapshot = try c.decodeIfPresent(SessionsSnapshot.self, forKey: .snapshot) {
            self = .snapshot(snapshot)
        } else if let change = try c.decodeIfPresent(SessionChange.self, forKey: .change) {
            self = .change(change)
        } else {
            self = .unknown
        }
    }
}

struct WatchSessionsRequest: Encodable {
    var afterEventId: ProtoInt64
}

struct HandshakeRequest: Encodable {
    var protocolVersion: UInt32
    var clientName: String
}

public struct HandshakeResponse: Decodable, Sendable, Equatable {
    public var protocolVersion: UInt32 = 0
    public var daemonVersion = ""

    enum CodingKeys: String, CodingKey { case protocolVersion, daemonVersion }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        protocolVersion = try c.value(.protocolVersion, 0)
        daemonVersion = try c.value(.daemonVersion, "")
    }
}

struct StatusRequest: Encodable {}

public struct StatusResponse: Decodable, Sendable, Equatable {
    public var state = ""
    public var stateVersion: Int64 = 0
    public var pid: Int32 = 0
    public var startedAt: Date?
    public var socketPath = ""
    public var daemonVersion = ""
    public var executable = ""

    enum CodingKeys: String, CodingKey {
        case state, stateVersion, pid, startedAt, socketPath, daemonVersion, executable
    }

    public init(pid: Int32, daemonVersion: String, executable: String) {
        self.pid = pid
        self.daemonVersion = daemonVersion
        self.executable = executable
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = try c.value(.state, "")
        stateVersion = try c.int64(.stateVersion)
        pid = try c.value(.pid, 0)
        startedAt = try c.date(.startedAt)
        socketPath = try c.value(.socketPath, "")
        daemonVersion = try c.value(.daemonVersion, "")
        executable = try c.value(.executable, "")
    }
}

public enum NeedKind: String, ProtoEnum {
    case unspecified = "NEED_KIND_UNSPECIFIED"
    case store = "NEED_KIND_STORE"
    case rotate = "NEED_KIND_ROTATE"
    case remove = "NEED_KIND_REMOVE"
    case grant = "NEED_KIND_GRANT"
    case leftover = "NEED_KIND_LEFTOVER"
}

public enum NeedEvent: String, ProtoEnum {
    case unspecified = "NEED_EVENT_UNSPECIFIED"
    case opened = "NEED_EVENT_OPENED"
    case resolved = "NEED_EVENT_RESOLVED"
}

public struct Need: Decodable, Sendable, Equatable, Identifiable {
    public var id = ""
    public var kind = NeedKind.unspecified
    public var secretName = ""
    public var ownerName = ""
    public var browser = BrowserKind.unspecified

    enum CodingKeys: String, CodingKey { case id, kind, secretName, ownerName, browser }

    public init(
        id: String, kind: NeedKind, secretName: String = "", ownerName: String = "", browser: BrowserKind = .unspecified
    ) {
        self.id = id
        self.kind = kind
        self.secretName = secretName
        self.ownerName = ownerName
        self.browser = browser
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.value(.id, "")
        kind = try c.value(.kind, .unspecified)
        secretName = try c.value(.secretName, "")
        ownerName = try c.value(.ownerName, "")
        browser = try c.value(.browser, .unspecified)
    }
}

public struct NeedsSnapshot: Decodable, Sendable, Equatable {
    public var eventID: Int64 = 0
    public var needs: [Need] = []
    public var reason = SnapshotReason.unspecified

    enum CodingKeys: String, CodingKey {
        case eventID = "eventId"
        case needs, reason
    }

    public init(eventID: Int64, needs: [Need], reason: SnapshotReason) {
        self.eventID = eventID
        self.needs = needs
        self.reason = reason
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        eventID = try c.int64(.eventID)
        needs = try c.value(.needs, [])
        reason = try c.value(.reason, .unspecified)
    }
}

public struct NeedChange: Decodable, Sendable, Equatable {
    public var eventID: Int64 = 0
    public var event = NeedEvent.unspecified
    public var need = Need(id: "", kind: .unspecified)

    enum CodingKeys: String, CodingKey {
        case eventID = "eventId"
        case event, need
    }

    public init(eventID: Int64, event: NeedEvent, need: Need) {
        self.eventID = eventID
        self.event = event
        self.need = need
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        eventID = try c.int64(.eventID)
        event = try c.value(.event, .unspecified)
        need = try c.value(.need, Need(id: "", kind: .unspecified))
    }
}

/// WatchNeedsResponse: a snapshot, a change, or the cursor past rows that changed no need; a
/// message holding none of them decodes as .unknown.
public enum WatchNeedsResponse: Decodable, Sendable, Equatable {
    case snapshot(NeedsSnapshot)
    case change(NeedChange)
    case cursor(Int64)
    case unknown

    enum CodingKeys: String, CodingKey { case snapshot, change, cursor }
    enum CursorKeys: String, CodingKey { case eventID = "eventId" }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        if let snapshot = try c.decodeIfPresent(NeedsSnapshot.self, forKey: .snapshot) {
            self = .snapshot(snapshot)
        } else if let change = try c.decodeIfPresent(NeedChange.self, forKey: .change) {
            self = .change(change)
        } else if c.contains(.cursor) {
            let cursor = try c.nestedContainer(keyedBy: CursorKeys.self, forKey: .cursor)
            self = .cursor(try cursor.int64(.eventID))
        } else {
            self = .unknown
        }
    }
}

struct WatchNeedsRequest: Encodable {
    var afterEventId: ProtoInt64
}
