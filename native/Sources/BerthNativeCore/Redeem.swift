import Foundation

public let redeemPath = "/berth.v1.SecretsService/RedeemUseTicket"

public enum RedeemError: Error, Equatable, CustomStringConvertible {
    case malformedResponse
    case status(Int)
    case connectError(String)
    case invalidName

    public var description: String {
        switch self {
        case .malformedResponse: "malformed HTTP response"
        case .status(let code): "berthd answered HTTP \(code)"
        case .connectError(let code): "berthd refused the ticket: \(code)"
        case .invalidName: "berthd returned an invalid secret name"
        }
    }
}

private struct RedeemBody: Encodable {
    var requestId: String
    var ticket: String
}

/// The Connect unary JSON call, as a complete HTTP/1.1 request. The proto field names are
/// camelCase in Connect JSON and bytes travel as standard base64.
public func redeemRequest(ticket: [UInt8], requestID: String) -> Data {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.sortedKeys]
    // Two plain strings always encode.
    let body =
        (try? encoder.encode(RedeemBody(requestId: requestID, ticket: Data(ticket).base64EncodedString())))
        ?? Data()
    let head = [
        "POST \(redeemPath) HTTP/1.1",
        "Host: localhost",
        "Content-Type: application/json",
        "Connect-Protocol-Version: 1",
        "Accept: application/json",
        "Content-Length: \(body.count)",
        "Connection: close",
        "",
        "",
    ].joined(separator: "\r\n")
    return Data(head.utf8) + body
}

public struct HTTPResponse: Equatable, Sendable {
    public var status: Int
    public var headers: [String: String]
    public var body: Data
}

/// Parses a whole HTTP/1.1 response read until the server closed the connection.
public func parseHTTPResponse(_ raw: Data) throws(RedeemError) -> HTTPResponse {
    let bytes = Array(raw)
    let separator: [UInt8] = [13, 10, 13, 10]
    guard let headEnd = firstIndex(of: separator, in: bytes, from: 0),
        let head = String(bytes: bytes[0..<headEnd], encoding: .utf8)
    else { throw .malformedResponse }
    let lines = head.components(separatedBy: "\r\n")
    let statusParts = lines[0].split(separator: " ", maxSplits: 2)
    guard statusParts.count >= 2, statusParts[0].hasPrefix("HTTP/1."), let status = Int(statusParts[1]) else {
        throw .malformedResponse
    }
    var headers: [String: String] = [:]
    for line in lines.dropFirst() {
        guard let colon = line.firstIndex(of: ":") else { throw .malformedResponse }
        let key = line[..<colon].trimmingCharacters(in: .whitespaces).lowercased()
        headers[key] = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
    }
    let rest = Array(bytes[(headEnd + separator.count)...])
    let body: [UInt8]
    if headers["transfer-encoding"]?.lowercased() == "chunked" {
        body = try dechunk(rest)
    } else if let lengthText = headers["content-length"] {
        guard let length = Int(lengthText), length >= 0, length <= rest.count else { throw .malformedResponse }
        body = Array(rest[0..<length])
    } else {
        body = rest
    }
    return HTTPResponse(status: status, headers: headers, body: Data(body))
}

private func dechunk(_ bytes: [UInt8]) throws(RedeemError) -> [UInt8] {
    var out: [UInt8] = []
    var i = 0
    while true {
        guard let lineEnd = firstIndex(of: [13, 10], in: bytes, from: i),
            let sizeLine = String(bytes: bytes[i..<lineEnd], encoding: .utf8),
            let size = Int(sizeLine.split(separator: ";")[0].trimmingCharacters(in: .whitespaces), radix: 16)
        else { throw .malformedResponse }
        i = lineEnd + 2
        if size == 0 { return out }
        guard i + size + 2 <= bytes.count, bytes[i + size] == 13, bytes[i + size + 1] == 10 else {
            throw .malformedResponse
        }
        out += bytes[i..<(i + size)]
        i += size + 2
    }
}

private func firstIndex(of needle: [UInt8], in haystack: [UInt8], from start: Int) -> Int? {
    guard haystack.count >= needle.count, start <= haystack.count - needle.count else { return nil }
    for i in start...(haystack.count - needle.count) where haystack[i..<(i + needle.count)].elementsEqual(needle) {
        return i
    }
    return nil
}

/// ConsumerKind in secrets.proto, as Connect JSON spells it.
public enum ConsumerKind: String, Sendable {
    case executable = "CONSUMER_KIND_EXECUTABLE"
    case proxy = "CONSUMER_KIND_PROXY"
}

/// TicketAction in secrets.proto, as Connect JSON spells it.
public enum TicketAction: String, Sendable {
    case use = "TICKET_ACTION_USE"
    case remove = "TICKET_ACTION_REMOVE"
    case rotate = "TICKET_ACTION_ROTATE"
}

/// What berthd said a ticket is for. A kind or action that is absent (the proto zero value) or
/// unknown is nil, so every rule below refuses it.
public struct RedeemedTicket: Equatable, Sendable {
    public var name: String
    public var consumerKind: ConsumerKind?
    public var action: TicketAction?

    public init(name: String, consumerKind: ConsumerKind?, action: TicketAction?) {
        self.name = name
        self.consumerKind = consumerKind
        self.action = action
    }
}

/// Returns what a RedeemUseTicket response grants, or why the redeem failed.
public func redeemedTicket(from response: HTTPResponse) throws(RedeemError) -> RedeemedTicket {
    let object = try? JSONSerialization.jsonObject(with: response.body) as? [String: Any]
    if let code = object?["code"] as? String { throw .connectError(code) }
    guard response.status == 200 else { throw .status(response.status) }
    guard let name = object?["name"] as? String else { throw .malformedResponse }
    guard isValidSecretName(name) else { throw .invalidName }
    return RedeemedTicket(
        name: name,
        consumerKind: (object?["consumerKind"] as? String).flatMap(ConsumerKind.init(rawValue:)),
        action: (object?["action"] as? String).flatMap(TicketAction.init(rawValue:)))
}

/// The receiver rule for read, applied after the redeem: the ticket must be for reading, and for
/// the kind of consumer the value channel's peer proved to be, so a ticket berthd made for the
/// proxy never reaches the berth CLI and the reverse.
public func useRefusal(parent: ConsumerKind, _ ticket: RedeemedTicket) -> String? {
    guard ticket.action == .use else { return "the ticket is not for reading a value" }
    guard ticket.consumerKind == parent else {
        return
            "the ticket is for \(ticket.consumerKind?.rawValue ?? "no consumer kind"), the parent is \(parent.rawValue)"
    }
    return nil
}

public enum LifecycleCommand: String, Sendable {
    case remove, rotate

    var action: TicketAction { self == .remove ? .remove : .rotate }
}

/// rotate and remove act only on a ticket berthd made for that command and that name.
public func lifecycleRefusal(_ command: LifecycleCommand, name: String, _ ticket: RedeemedTicket) -> String? {
    guard ticket.action == command.action else { return "the ticket is not for \(command.rawValue)" }
    guard ticket.name == name else { return "the ticket is for another secret" }
    return nil
}
