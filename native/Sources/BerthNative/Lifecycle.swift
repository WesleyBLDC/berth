import BerthNativeCore
import Foundation

@MainActor
func runRemove(_ line: Data) async -> Never {
    let (request, ticket) = decodeLifecycleOrExit(line)
    let answer = await remove(
        request,
        steps: RemoveSteps(
            redeem: { try redeem(ticket) },
            confirm: { await askTouchID(reason: $0) },
            delete: { Keychain.delete($0) }))
    reply(answer.outcome, answer.error)
}

@MainActor
func runRotate(_ line: Data) async -> Never {
    let (request, ticket) = decodeLifecycleOrExit(line)
    let answer = await rotate(
        request,
        steps: RotateSteps<SecretBuffer>(
            redeem: { try redeem(ticket) },
            exists: { Keychain.exists($0) },
            askValue: { askForValue($0) },
            confirm: { await askTouchID(reason: $0) },
            update: { Keychain.update($0, value: $1) },
            wipe: { $0.wipe() }))
    reply(answer.outcome, answer.error)
}

private func decodeLifecycleOrExit(_ line: Data) -> (LifecycleRequest, [UInt8]) {
    do {
        return try decodeLifecycle(line)
    } catch {
        misuse(error.description)
    }
}
