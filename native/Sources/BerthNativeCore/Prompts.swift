import Foundation

public struct DialogText: Equatable, Sendable {
    public var title: String
    public var body: String
}

/// The store dialog. Every fact comes from the request berthd verified; the reason is the
/// requester's own claim, so it only ever appears quoted and labelled, never as the title.
public func storeDialogText(_ request: StoreRequest) -> DialogText {
    let requester = request.requester
    var lines = [
        "Requested by: \(oneLine(requester.executable)) (pid \(requester.pid))",
        "Working directory: \(oneLine(requester.workingDir))",
    ]
    if !requester.tree.isEmpty {
        lines.append("Process tree:")
        lines += requester.tree.map { "  \(oneLine($0.executable)) (pid \($0.pid))" }
    }
    lines += [
        "",
        "The requester wrote: \(quoted(request.reason))",
        "",
        "Berth saves the value in your login keychain as \(keychainService(for: request.name)). "
            + "The requester learns only whether it was saved.",
    ]
    return DialogText(title: "Store a secret named \(request.name)?", body: lines.joined(separator: "\n"))
}

/// Touch ID shows this after "berth-native is trying to", so it reads as a verb phrase.
public func storeTouchIDReason(_ request: StoreRequest) -> String {
    "save secret \(request.name) for \(oneLine(request.requester.executable))"
}

public func confirmTouchIDReason(_ request: ConfirmRequest) -> String {
    let consumer = request.consumer
    var facts = ["sha256 \(consumer.sha256.prefix(12))"]
    if !consumer.teamID.isEmpty { facts.insert("team \(oneLine(consumer.teamID))", at: 0) }
    facts.append(request.argsGlob.isEmpty ? "any arguments" : "arguments \(quoted(request.argsGlob))")
    return "let \(oneLine(consumer.path)) (\(facts.joined(separator: ", "))) use secret \(request.secret), "
        + "requested by \(oneLine(request.requester.executable))"
}

/// Agent controlled text is folded onto one line and capped, so it cannot fake extra lines
/// that look like Berth's own facts.
func oneLine(_ text: String, limit: Int = 500) -> String {
    let folded = String(
        String.UnicodeScalarView(
            text.unicodeScalars.map { scalar in
                CharacterSet.controlCharacters.contains(scalar) || CharacterSet.newlines.contains(scalar) ? " " : scalar
            }))
    return folded.count > limit ? String(folded.prefix(limit)) + "\u{2026}" : folded
}

func quoted(_ text: String) -> String { "\u{201C}\(oneLine(text))\u{201D}" }
