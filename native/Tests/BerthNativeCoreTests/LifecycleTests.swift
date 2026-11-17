import Foundation
import Security
import Testing

@testable import BerthNativeCore

let lifecycleRequest = LifecycleRequest(name: "deploy-key", ticket: ticketHex, requester: requester)

struct RedeemFailed: Error {}

/// Records every side effect in order, so a test can prove nothing ran before the redeem passed.
@MainActor
final class Effects {
    var calls: [String] = []
    var touchID = TouchIDResult.approved
    var status: OSStatus = errSecSuccess
    var exists = true
    var answer = ValueAnswer<String>.entered("canary-5b1e")

    func removeSteps(redeem: @escaping @MainActor () throws -> RedeemedTicket) -> RemoveSteps {
        RemoveSteps(
            redeem: {
                self.calls.append("redeem"); return try redeem()
            },
            confirm: {
                self.calls.append("touchid \($0)"); return self.touchID
            },
            delete: {
                self.calls.append("delete \($0)"); return self.status
            })
    }

    func rotateSteps(redeem: @escaping @MainActor () throws -> RedeemedTicket) -> RotateSteps<String> {
        RotateSteps(
            redeem: {
                self.calls.append("redeem"); return try redeem()
            },
            exists: {
                self.calls.append("exists \($0)"); return self.exists
            },
            askValue: {
                self.calls.append("dialog \($0.title)"); return self.answer
            },
            confirm: { _ in
                self.calls.append("touchid"); return self.touchID
            },
            update: { name, _ in
                self.calls.append("update \(name)"); return self.status
            },
            wipe: { self.calls.append("wipe \($0)") })
    }
}

let goodRemove = { @MainActor () throws -> RedeemedTicket in
    RedeemedTicket(name: "deploy-key", consumerKind: nil, action: .remove)
}
let goodRotate = { @MainActor () throws -> RedeemedTicket in
    RedeemedTicket(name: "deploy-key", consumerKind: nil, action: .rotate)
}

/// Every way the redeem can fail: berthd refuses, or the ticket is for another command or name.
let badRedeems: [@MainActor () throws -> RedeemedTicket] = [
    { throw RedeemFailed() },
    { RedeemedTicket(name: "deploy-key", consumerKind: nil, action: .use) },
    { RedeemedTicket(name: "other", consumerKind: nil, action: .remove) },
    { RedeemedTicket(name: "other", consumerKind: nil, action: .rotate) },
    { RedeemedTicket(name: "deploy-key", consumerKind: .executable, action: nil) },
]

@MainActor @Test func removeRedeemsBeforeAnyPrompt() async {
    for bad in badRedeems {
        let effects = Effects()
        let answer = await remove(lifecycleRequest, steps: effects.removeSteps(redeem: bad))
        #expect(answer.outcome == .failed)
        #expect(effects.calls == ["redeem"])
    }
}

@MainActor @Test func rotateRedeemsBeforeAnyPrompt() async {
    for bad in badRedeems {
        let effects = Effects()
        let answer = await rotate(lifecycleRequest, steps: effects.rotateSteps(redeem: bad))
        #expect(answer.outcome == .failed)
        #expect(effects.calls == ["redeem"])
    }
}

@MainActor @Test func removeAsksTouchIDThenDeletes() async {
    let effects = Effects()
    let answer = await remove(lifecycleRequest, steps: effects.removeSteps(redeem: goodRemove))
    #expect(answer.outcome == .removed && answer.error == nil)
    #expect(effects.calls == ["redeem", "touchid \(removeTouchIDReason(lifecycleRequest))", "delete deploy-key"])
}

@MainActor @Test func removeTreatsNotFoundAsRemoved() async {
    let effects = Effects()
    effects.status = errSecItemNotFound
    #expect(await remove(lifecycleRequest, steps: effects.removeSteps(redeem: goodRemove)).outcome == .removed)
    effects.status = errSecAuthFailed
    let failed = await remove(lifecycleRequest, steps: effects.removeSteps(redeem: goodRemove))
    #expect(failed.outcome == .failed && failed.error?.contains("\(errSecAuthFailed)") == true)
}

@MainActor @Test func removeDeniedOrFailedTouchIDDeletesNothing() async {
    for (touchID, outcome) in [(TouchIDResult.denied, RemoveOutcome.denied), (.failed("no sensor"), .failed)] {
        let effects = Effects()
        effects.touchID = touchID
        let answer = await remove(lifecycleRequest, steps: effects.removeSteps(redeem: goodRemove))
        #expect(answer.outcome == outcome)
        #expect(!effects.calls.contains { $0.hasPrefix("delete") })
    }
}

@MainActor @Test func rotateShowsReplaceDialogThenTouchIDThenUpdatesAndWipes() async {
    let effects = Effects()
    let answer = await rotate(lifecycleRequest, steps: effects.rotateSteps(redeem: goodRotate))
    #expect(answer.outcome == .stored && answer.error == nil)
    #expect(
        effects.calls == [
            "redeem", "exists deploy-key", "dialog Replace the secret named deploy-key?", "touchid",
            "update deploy-key",
            "wipe canary-5b1e",
        ])
}

@MainActor @Test func rotateOfAMissingItemFailsBeforeTheDialog() async {
    let effects = Effects()
    effects.exists = false
    let answer = await rotate(lifecycleRequest, steps: effects.rotateSteps(redeem: goodRotate))
    #expect(answer.outcome == .failed && answer.error == "berth.deploy-key does not exist")
    #expect(effects.calls == ["redeem", "exists deploy-key"])
}

@MainActor @Test func rotateWipesTheValueWhenTouchIDSaysNo() async {
    for (touchID, outcome) in [(TouchIDResult.denied, StoreOutcome.denied), (.failed("x"), .failed)] {
        let effects = Effects()
        effects.touchID = touchID
        let answer = await rotate(lifecycleRequest, steps: effects.rotateSteps(redeem: goodRotate))
        #expect(answer.outcome == outcome)
        #expect(!effects.calls.contains { $0.hasPrefix("update") })
        #expect(effects.calls.last == "wipe canary-5b1e")
    }
}

@MainActor @Test func rotateCancelOrEmptyNeverAsksTouchID() async {
    for (dialog, outcome) in [(ValueAnswer<String>.cancelled, StoreOutcome.denied), (.empty, .failed)] {
        let effects = Effects()
        effects.answer = dialog
        let answer = await rotate(lifecycleRequest, steps: effects.rotateSteps(redeem: goodRotate))
        #expect(answer.outcome == outcome)
        #expect(!effects.calls.contains("touchid"))
    }
}

@Test func decodesLifecycleRequest() throws {
    let line =
        #"{"name":"deploy-key","ticket":"\#(ticketHex)","requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}}"#
    let (request, ticket) = try decodeLifecycle(Data((line + "\n").utf8))
    #expect(request.name == "deploy-key")
    #expect(ticket == Array(0..<32))
}

@Test(arguments: ["", ticketHex + "\n", ticketHex.uppercased(), String(ticketHex.dropLast()), ticketHex + "00"])
func rejectsBadLifecycleTicket(_ ticket: String) throws {
    let body = try JSONEncoder().encode(
        LifecycleRequest(
            name: "k", ticket: ticket, requester: Requester(pid: 1, executable: "/x", tree: [], workingDir: "/")))
    #expect(throws: ProtocolError.invalidTicket) { try decodeLifecycle(body) }
}

@Test func rejectsBadLifecycleName() {
    let line =
        #"{"name":"../x","ticket":"\#(ticketHex)","requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}}"#
    #expect(throws: ProtocolError.invalidName) { try decodeLifecycle(Data(line.utf8)) }
}
