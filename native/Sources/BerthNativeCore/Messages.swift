import Foundation

public struct TreeProcess: Codable, Equatable, Sendable {
    public var pid: Int
    public var executable: String

    public init(pid: Int, executable: String) {
        self.pid = pid
        self.executable = executable
    }
}

public struct Requester: Codable, Equatable, Sendable {
    public var pid: Int
    public var executable: String
    public var tree: [TreeProcess]
    public var workingDir: String

    enum CodingKeys: String, CodingKey {
        case pid, executable, tree
        case workingDir = "working_dir"
    }

    public init(pid: Int, executable: String, tree: [TreeProcess], workingDir: String) {
        self.pid = pid
        self.executable = executable
        self.tree = tree
        self.workingDir = workingDir
    }
}

public struct StoreRequest: Codable, Equatable, Sendable {
    public var name: String
    public var reason: String
    public var requester: Requester

    public init(name: String, reason: String, requester: Requester) {
        self.name = name
        self.reason = reason
        self.requester = requester
    }
}

public struct Consumer: Codable, Equatable, Sendable {
    public var path: String
    public var sha256: String
    public var teamID: String
    public var signingID: String

    enum CodingKeys: String, CodingKey {
        case path, sha256
        case teamID = "team_id"
        case signingID = "signing_id"
    }

    public init(path: String, sha256: String, teamID: String, signingID: String) {
        self.path = path
        self.sha256 = sha256
        self.teamID = teamID
        self.signingID = signingID
    }
}

public struct ConfirmRequest: Codable, Equatable, Sendable {
    public var action: String
    public var secret: String
    public var consumer: Consumer
    public var argsGlob: String
    /// True when berthd found the consumer is an interpreter, which could print the secret.
    /// Absent means false.
    public var interpreter: Bool
    /// Environment variables the consumer will also receive; absent means none.
    public var passEnv: [String]
    public var requester: Requester

    enum CodingKeys: String, CodingKey {
        case action, secret, consumer, requester, interpreter
        case argsGlob = "args_glob"
        case passEnv = "pass_env"
    }

    public init(
        action: String, secret: String, consumer: Consumer, argsGlob: String, interpreter: Bool = false,
        passEnv: [String] = [], requester: Requester
    ) {
        self.action = action
        self.secret = secret
        self.consumer = consumer
        self.argsGlob = argsGlob
        self.interpreter = interpreter
        self.passEnv = passEnv
        self.requester = requester
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        action = try c.decode(String.self, forKey: .action)
        secret = try c.decode(String.self, forKey: .secret)
        consumer = try c.decode(Consumer.self, forKey: .consumer)
        argsGlob = try c.decode(String.self, forKey: .argsGlob)
        interpreter = try c.decodeIfPresent(Bool.self, forKey: .interpreter) ?? false
        passEnv = try c.decodeIfPresent([String].self, forKey: .passEnv) ?? []
        requester = try c.decode(Requester.self, forKey: .requester)
    }
}

/// The one header on one HTTPS host and port berth-proxy may write the value to, as berthd
/// verified it. Every string is still text the requester chose, so prompts fold and cap it.
public struct ProxyTarget: Codable, Equatable, Sendable {
    public var host: String
    public var port: Int
    public var headerName: String
    public var valuePrefix: String
    public var valueSuffix: String
    /// Empty means any path.
    public var pathGlob: String
    /// Empty means any method.
    public var methods: [String]
    /// Empty means the child gets no placeholder variable.
    public var placeholderEnv: String

    enum CodingKeys: String, CodingKey {
        case host, port, methods
        case headerName = "header_name"
        case valuePrefix = "value_prefix"
        case valueSuffix = "value_suffix"
        case pathGlob = "path_glob"
        case placeholderEnv = "placeholder_env"
    }

    public init(
        host: String, port: Int, headerName: String, valuePrefix: String = "", valueSuffix: String = "",
        pathGlob: String = "", methods: [String] = [], placeholderEnv: String = ""
    ) {
        self.host = host
        self.port = port
        self.headerName = headerName
        self.valuePrefix = valuePrefix
        self.valueSuffix = valueSuffix
        self.pathGlob = pathGlob
        self.methods = methods
        self.placeholderEnv = placeholderEnv
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        host = try c.decode(String.self, forKey: .host)
        port = try c.decode(Int.self, forKey: .port)
        headerName = try c.decode(String.self, forKey: .headerName)
        valuePrefix = try c.decodeIfPresent(String.self, forKey: .valuePrefix) ?? ""
        valueSuffix = try c.decodeIfPresent(String.self, forKey: .valueSuffix) ?? ""
        pathGlob = try c.decodeIfPresent(String.self, forKey: .pathGlob) ?? ""
        methods = try c.decodeIfPresent([String].self, forKey: .methods) ?? []
        placeholderEnv = try c.decodeIfPresent(String.self, forKey: .placeholderEnv) ?? ""
    }
}

/// confirm with "action":"proxy": let berth-proxy send secret to target.
public struct ProxyConfirmRequest: Codable, Equatable, Sendable {
    public var action: String
    public var secret: String
    public var target: ProxyTarget
    public var requester: Requester

    public init(secret: String, target: ProxyTarget, requester: Requester) {
        action = "proxy"
        self.secret = secret
        self.target = target
        self.requester = requester
    }
}

/// What a confirm line asks for, chosen by its action.
public enum ConfirmPrompt: Equatable, Sendable {
    case grant(ConfirmRequest)
    case proxy(ProxyConfirmRequest)
}

/// The stdin of rotate and remove: the action ticket berthd made for this one command, and the
/// facts the prompts show.
public struct LifecycleRequest: Codable, Equatable, Sendable {
    public var name: String
    public var ticket: String
    public var requester: Requester

    public init(name: String, ticket: String, requester: Requester) {
        self.name = name
        self.ticket = ticket
        self.requester = requester
    }
}

public enum RemoveOutcome: String, Codable, Sendable {
    case removed, denied, failed
}

public enum StoreOutcome: String, Codable, Sendable {
    case stored, denied, failed
}

public enum ConfirmOutcome: String, Codable, Sendable {
    case approved, denied, failed
}

public struct Reply<Outcome: Codable & Sendable>: Codable, Sendable {
    public var outcome: Outcome
    public var error: String?

    public init(_ outcome: Outcome, error: String? = nil) {
        self.outcome = outcome
        self.error = error
    }
}

public enum ProtocolError: Error, Equatable, CustomStringConvertible {
    case badJSON
    case invalidName
    case invalidSHA256
    case unknownAction(String)
    case invalidTarget
    case invalidTicket

    public var description: String {
        switch self {
        case .badJSON: "stdin is not the expected JSON object"
        case .invalidName: "invalid secret name"
        case .invalidSHA256: "consumer sha256 is not 64 lowercase hex characters"
        case .unknownAction(let action): "unknown action \(action)"
        case .invalidTarget: "proxy target needs a host, a header name and a port from 1 to 65535"
        case .invalidTicket: "ticket is not 64 lowercase hex characters"
        }
    }
}

/// Mirrors the name pattern in secrets.proto, so a name can never escape the berth. service prefix.
public func isValidSecretName(_ name: String) -> Bool {
    let bytes = Array(name.utf8)
    guard let first = bytes.first, bytes.count <= 128, isLowerAlnum(first) else { return false }
    return bytes.allSatisfy {
        isLowerAlnum($0) || $0 == UInt8(ascii: ".") || $0 == UInt8(ascii: "_") || $0 == UInt8(ascii: "-")
    }
}

public func keychainService(for name: String) -> String { "berth.\(name)" }

public let keychainAccount = "berth"

func isLowerAlnum(_ byte: UInt8) -> Bool {
    (UInt8(ascii: "a")...UInt8(ascii: "z")).contains(byte) || (UInt8(ascii: "0")...UInt8(ascii: "9")).contains(byte)
}

func isLowerHex(_ byte: UInt8) -> Bool {
    (UInt8(ascii: "a")...UInt8(ascii: "f")).contains(byte) || (UInt8(ascii: "0")...UInt8(ascii: "9")).contains(byte)
}

/// The protocol sends one JSON object on one line; a trailing newline is allowed.
public func decodeStore(_ data: Data) throws(ProtocolError) -> StoreRequest {
    let request: StoreRequest = try decode(data)
    guard isValidSecretName(request.name) else { throw .invalidName }
    return request
}

public func decodeConfirm(_ data: Data) throws(ProtocolError) -> ConfirmPrompt {
    struct Action: Decodable { var action: String }
    let action: Action = try decode(data)
    switch action.action {
    case "grant":
        let request: ConfirmRequest = try decode(data)
        guard isValidSecretName(request.secret) else { throw .invalidName }
        let hash = Array(request.consumer.sha256.utf8)
        guard hash.count == 64, hash.allSatisfy(isLowerHex) else { throw .invalidSHA256 }
        return .grant(request)
    case "proxy":
        let request: ProxyConfirmRequest = try decode(data)
        guard isValidSecretName(request.secret) else { throw .invalidName }
        let target = request.target
        guard !target.host.isEmpty, !target.headerName.isEmpty, (1...65535).contains(target.port) else {
            throw .invalidTarget
        }
        return .proxy(request)
    default:
        throw .unknownAction(action.action)
    }
}

/// Returns the request and its ticket bytes; the ticket is checked before anything is redeemed.
public func decodeLifecycle(_ data: Data) throws(ProtocolError) -> (LifecycleRequest, [UInt8]) {
    let request: LifecycleRequest = try decode(data)
    guard isValidSecretName(request.name) else { throw .invalidName }
    guard request.ticket.utf8.count == 64, let ticket = try? parseTicket(Data(request.ticket.utf8)) else {
        throw .invalidTicket
    }
    return (request, ticket)
}

private func decode<T: Decodable>(_ data: Data) throws(ProtocolError) -> T {
    do {
        return try JSONDecoder().decode(T.self, from: data)
    } catch {
        throw .badJSON
    }
}

/// One JSON object and a newline, the only thing store and confirm ever write to stdout.
public func encodeReply<Outcome>(_ reply: Reply<Outcome>) -> Data {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
    // Encoding two string fields cannot fail; the fallback keeps the line well formed anyway.
    var line = (try? encoder.encode(reply)) ?? Data(#"{"outcome":"failed"}"#.utf8)
    line.append(UInt8(ascii: "\n"))
    return line
}
