import BerthNativeCore
import Foundation

@MainActor
func runConfirm(_ line: Data) async -> Never {
    let request: ConfirmPrompt
    do {
        request = try decodeConfirm(line)
    } catch {
        misuse(error.description)
    }
    switch await askTouchID(reason: confirmTouchIDReason(request)) {
    case .approved: reply(ConfirmOutcome.approved)
    case .denied: reply(ConfirmOutcome.denied)
    case .failed(let why): reply(ConfirmOutcome.failed, why)
    }
}
