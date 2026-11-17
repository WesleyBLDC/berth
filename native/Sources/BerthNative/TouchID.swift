import Foundation
import LocalAuthentication

enum TouchIDResult: Equatable {
    case approved
    case denied
    case failed(String)
}

/// Touch ID, or the login password when Touch ID is unavailable (deviceOwnerAuthentication).
func askTouchID(reason: String) async -> TouchIDResult {
    let context = LAContext()
    var error: NSError?
    guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else {
        return .failed("device owner authentication unavailable: \(error?.code ?? 0)")
    }
    do {
        try await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)
        return .approved
    } catch let error as LAError {
        switch error.code {
        case .userCancel, .appCancel, .systemCancel, .userFallback, .authenticationFailed:
            return .denied
        default:
            return .failed("touch id error \(error.code.rawValue)")
        }
    } catch {
        return .failed("touch id error")
    }
}
