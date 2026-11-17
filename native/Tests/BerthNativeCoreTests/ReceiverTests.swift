import Foundation
import Testing

@testable import BerthNativeCore

struct UseCase: Sendable, CustomTestStringConvertible {
    var parent: ConsumerKind
    var kind: ConsumerKind?
    var action: TicketAction?
    var allowed: Bool

    var testDescription: String {
        "\(parent) parent, kind \(kind?.rawValue ?? "none"), action \(action?.rawValue ?? "none")"
    }
}

@Test(arguments: [
    UseCase(parent: .executable, kind: .executable, action: .use, allowed: true),
    UseCase(parent: .proxy, kind: .proxy, action: .use, allowed: true),
    UseCase(parent: .executable, kind: .proxy, action: .use, allowed: false),
    UseCase(parent: .proxy, kind: .executable, action: .use, allowed: false),
    UseCase(parent: .executable, kind: nil, action: .use, allowed: false),
    UseCase(parent: .proxy, kind: nil, action: .use, allowed: false),
    UseCase(parent: .executable, kind: .executable, action: .remove, allowed: false),
    UseCase(parent: .proxy, kind: .proxy, action: .rotate, allowed: false),
    UseCase(parent: .executable, kind: .executable, action: nil, allowed: false),
])
func readReceiverTable(_ c: UseCase) {
    let refusal = useRefusal(parent: c.parent, RedeemedTicket(name: "k", consumerKind: c.kind, action: c.action))
    #expect((refusal == nil) == c.allowed, "\(refusal ?? "allowed")")
}

@Test func lifecycleTicketMustMatchCommandAndName() {
    func ticket(_ name: String, _ action: TicketAction?) -> RedeemedTicket {
        RedeemedTicket(name: name, consumerKind: nil, action: action)
    }
    #expect(lifecycleRefusal(.remove, name: "k", ticket("k", .remove)) == nil)
    #expect(lifecycleRefusal(.rotate, name: "k", ticket("k", .rotate)) == nil)
    #expect(lifecycleRefusal(.remove, name: "k", ticket("k", .rotate)) == "the ticket is not for remove")
    #expect(lifecycleRefusal(.rotate, name: "k", ticket("k", .remove)) == "the ticket is not for rotate")
    #expect(lifecycleRefusal(.remove, name: "k", ticket("k", .use)) != nil)
    #expect(lifecycleRefusal(.rotate, name: "k", ticket("k", nil)) != nil)
    #expect(lifecycleRefusal(.remove, name: "k", ticket("other", .remove)) == "the ticket is for another secret")
}

@Test func redeemedTicketReadsConnectEnumNames() throws {
    let body = #"{"name":"k","consumerKind":"CONSUMER_KIND_PROXY","action":"TICKET_ACTION_USE"}"#
    let parsed = try redeemedTicket(from: parseHTTPResponse(response("200 OK", body: body)))
    #expect(parsed == RedeemedTicket(name: "k", consumerKind: .proxy, action: .use))
    let remove = try redeemedTicket(
        from: parseHTTPResponse(response("200 OK", body: #"{"name":"k","action":"TICKET_ACTION_REMOVE"}"#)))
    #expect(remove == RedeemedTicket(name: "k", consumerKind: nil, action: .remove))
}

@Test(arguments: [
    #"{"name":"k"}"#, #"{"name":"k","consumerKind":2,"action":1}"#,
    #"{"name":"k","consumerKind":"CONSUMER_KIND_UNSPECIFIED","action":"TICKET_ACTION_UNSPECIFIED"}"#,
    #"{"name":"k","consumerKind":"consumer_kind_proxy","action":"use"}"#,
])
func absentOrUnknownKindAndActionAreNil(_ body: String) throws {
    let parsed = try redeemedTicket(from: parseHTTPResponse(response("200 OK", body: body)))
    #expect(parsed == RedeemedTicket(name: "k", consumerKind: nil, action: nil))
    #expect(useRefusal(parent: .executable, parsed) != nil)
}

@Test func proxyParentRequirementPinsTheSignedProxy() {
    #expect(berthProxyRequirement.hasPrefix("anchor apple generic"))
    #expect(berthProxyRequirement.contains(#"leaf[subject.OU] = "XPJ7XA42J4""#))
    #expect(berthProxyRequirement.contains(#"identifier "com.wesleybldc.berth.proxy""#))
    #expect(parentRequirements.map(\.0) == [.executable, .proxy])
    #expect(parentRequirements.map(\.1) == [berthCLIRequirement, berthProxyRequirement])
}
