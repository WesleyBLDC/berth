import Foundation
import Security

public enum TouchIDResult: Equatable, Sendable {
    case approved
    case denied
    case failed(String)
}

public struct OSStatusError: Error, CustomStringConvertible {
    public var status: OSStatus

    public init(status: OSStatus) { self.status = status }

    public var description: String {
        let message = SecCopyErrorMessageString(status, nil) as String? ?? "unknown"
        return "keychain status \(status) (\(message))"
    }
}

public enum ValueAnswer<Value> {
    case entered(Value)
    case empty
    case cancelled
}

/// The side effects of remove, injected so tests can prove the order without Touch ID or the
/// Keychain.
@MainActor
public struct RemoveSteps {
    public var redeem: @MainActor () throws -> RedeemedTicket
    public var confirm: @MainActor (String) async -> TouchIDResult
    public var delete: @MainActor (String) -> OSStatus

    public init(
        redeem: @escaping @MainActor () throws -> RedeemedTicket,
        confirm: @escaping @MainActor (String) async -> TouchIDResult,
        delete: @escaping @MainActor (String) -> OSStatus
    ) {
        self.redeem = redeem
        self.confirm = confirm
        self.delete = delete
    }
}

/// The ticket is redeemed and checked before Touch ID, so only berthd, which made the ticket for
/// this name and command, can put a delete prompt in front of the person.
@MainActor
public func remove(_ request: LifecycleRequest, steps: RemoveSteps) async -> Reply<RemoveOutcome> {
    if let refusal = redeemRefusal(.remove, request, steps.redeem) { return Reply(.failed, error: refusal) }
    switch await steps.confirm(removeTouchIDReason(request)) {
    case .approved: break
    case .denied: return Reply(.denied)
    case .failed(let why): return Reply(.failed, error: why)
    }
    switch steps.delete(request.name) {
    // Already gone is what the person asked for, and lets berthd finish revoking the grants.
    case errSecSuccess, errSecItemNotFound: return Reply(.removed)
    case let status: return Reply(.failed, error: OSStatusError(status: status).description)
    }
}

@MainActor
public struct RotateSteps<Value> {
    public var redeem: @MainActor () throws -> RedeemedTicket
    public var exists: @MainActor (String) -> Bool
    public var askValue: @MainActor (DialogText) -> ValueAnswer<Value>
    public var confirm: @MainActor (String) async -> TouchIDResult
    public var update: @MainActor (String, Value) -> OSStatus
    public var wipe: @MainActor (Value) -> Void

    public init(
        redeem: @escaping @MainActor () throws -> RedeemedTicket,
        exists: @escaping @MainActor (String) -> Bool,
        askValue: @escaping @MainActor (DialogText) -> ValueAnswer<Value>,
        confirm: @escaping @MainActor (String) async -> TouchIDResult,
        update: @escaping @MainActor (String, Value) -> OSStatus,
        wipe: @escaping @MainActor (Value) -> Void
    ) {
        self.redeem = redeem
        self.exists = exists
        self.askValue = askValue
        self.confirm = confirm
        self.update = update
        self.wipe = wipe
    }
}

@MainActor
public func rotate<Value>(_ request: LifecycleRequest, steps: RotateSteps<Value>) async -> Reply<StoreOutcome> {
    if let refusal = redeemRefusal(.rotate, request, steps.redeem) { return Reply(.failed, error: refusal) }
    let service = keychainService(for: request.name)
    // Checked before the dialog so the person never types a value that cannot be saved.
    guard steps.exists(request.name) else { return Reply(.failed, error: "\(service) does not exist") }
    let value: Value
    switch steps.askValue(rotateDialogText(request)) {
    case .entered(let entered): value = entered
    case .empty: return Reply(.failed, error: "empty value")
    case .cancelled: return Reply(.denied)
    }
    defer { steps.wipe(value) }
    switch await steps.confirm(rotateTouchIDReason(request)) {
    case .approved: break
    case .denied: return Reply(.denied)
    case .failed(let why): return Reply(.failed, error: why)
    }
    switch steps.update(request.name, value) {
    case errSecSuccess: return Reply(.stored)
    case errSecItemNotFound: return Reply(.failed, error: "\(service) does not exist")
    case let status: return Reply(.failed, error: OSStatusError(status: status).description)
    }
}

@MainActor
private func redeemRefusal(
    _ command: LifecycleCommand, _ request: LifecycleRequest, _ redeem: @MainActor () throws -> RedeemedTicket
) -> String? {
    do {
        return lifecycleRefusal(command, name: request.name, try redeem())
    } catch {
        return "redeem: \(error)"
    }
}
