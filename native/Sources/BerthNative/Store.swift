import AppKit
import BerthNativeCore
import Foundation

@MainActor
func runStore(_ line: Data) async -> Never {
    let request: StoreRequest
    do {
        request = try decodeStore(line)
    } catch {
        misuse(error.description)
    }
    // Checked before the dialog so the person never types a value that cannot be saved.
    if Keychain.exists(request.name) {
        reply(StoreOutcome.failed, "\(keychainService(for: request.name)) already exists")
    }
    let answer = askForValue(storeDialogText(request))
    guard case .entered(let value) = answer else {
        if answer == .empty { reply(StoreOutcome.failed, "empty value") }
        reply(StoreOutcome.denied)
    }
    switch await askTouchID(reason: storeTouchIDReason(request)) {
    case .approved: break
    case .denied:
        value.wipe()
        reply(StoreOutcome.denied)
    case .failed(let why):
        value.wipe()
        reply(StoreOutcome.failed, why)
    }
    let status = Keychain.add(request.name, value: value)
    value.wipe()
    switch status {
    case errSecSuccess: reply(StoreOutcome.stored)
    case errSecDuplicateItem: reply(StoreOutcome.failed, "\(keychainService(for: request.name)) already exists")
    default: reply(StoreOutcome.failed, OSStatusError(status: status).description)
    }
}

enum DialogAnswer: Equatable {
    case entered(SecretBuffer)
    case empty
    case cancelled

    static func == (lhs: DialogAnswer, rhs: DialogAnswer) -> Bool {
        switch (lhs, rhs) {
        case (.empty, .empty), (.cancelled, .cancelled): true
        case (.entered(let a), .entered(let b)): a === b
        default: false
        }
    }
}

@MainActor
func askForValue(_ text: DialogText) -> DialogAnswer {
    let app = NSApplication.shared
    // A command line process needs a window server connection and a policy that may show
    // windows without a Dock icon.
    app.setActivationPolicy(.accessory)
    let dialog = StorePanel(text)
    let field = dialog.field
    app.activate()
    let confirmed = dialog.run()
    defer { field.stringValue = "" }
    guard confirmed else { return .cancelled }
    guard let value = SecretBuffer(copying: field.stringValue) else { return .empty }
    return .entered(value)
}
