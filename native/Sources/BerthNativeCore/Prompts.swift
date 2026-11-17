import Foundation

public struct DialogText: Equatable, Sendable {
    public var title: String
    public var body: String
    /// The label of the brass confirm button.
    public var button = "Save"
}

/// The store dialog. Every fact comes from the request berthd verified; the reason is the
/// requester's own claim, so it only ever appears quoted and labelled, never as the title.
public func storeDialogText(_ request: StoreRequest) -> DialogText {
    var lines = requesterLines(request.requester)
    lines += [
        "",
        "The requester wrote: \(quoted(request.reason))",
        "",
        "Berth saves the value in your login keychain as \(keychainService(for: request.name)). "
            + "The requester learns only whether it was saved.",
    ]
    return DialogText(title: "Store a secret named \(request.name)?", body: lines.joined(separator: "\n"))
}

/// The rotate dialog: the store panel in replace mode, with no requester claim at all.
public func rotateDialogText(_ request: LifecycleRequest) -> DialogText {
    let lines =
        requesterLines(request.requester) + [
            "",
            "Berth replaces the value of \(keychainService(for: request.name)) in your login keychain. "
                + "Grants stay active; running proxies keep the old value until they end. "
                + "The requester learns only whether it was replaced.",
        ]
    return DialogText(
        title: "Replace the secret named \(request.name)?", body: lines.joined(separator: "\n"), button: "Replace")
}

private func requesterLines(_ requester: Requester) -> [String] {
    var lines = [
        "Requested by: \(oneLine(requester.executable)) (pid \(requester.pid))",
        "Working directory: \(oneLine(requester.workingDir))",
    ]
    if !requester.tree.isEmpty {
        lines.append("Process tree:")
        lines += requester.tree.map { "  \(oneLine($0.executable)) (pid \($0.pid))" }
    }
    return lines
}

/// Touch ID shows this after "berth-native is trying to", so it reads as a verb phrase.
public func storeTouchIDReason(_ request: StoreRequest) -> String {
    "save secret \(request.name) for \(oneLine(request.requester.executable))"
}

public func rotateTouchIDReason(_ request: LifecycleRequest) -> String {
    "replace secret \(request.name) in your login keychain, requested by \(requestedBy(request.requester))"
}

public func removeTouchIDReason(_ request: LifecycleRequest) -> String {
    "delete secret \(request.name) from your login keychain and revoke its grants, requested by "
        + requestedBy(request.requester)
}

private func requestedBy(_ requester: Requester) -> String {
    "\(oneLine(requester.executable)) (pid \(requester.pid))"
}

public func confirmTouchIDReason(_ prompt: ConfirmPrompt) -> String {
    switch prompt {
    case .grant(let request): confirmTouchIDReason(request)
    case .proxy(let request): proxyTouchIDReason(request)
    }
}

/// Every target field is text the requester chose when it asked for the grant, so each is folded
/// onto one line and capped; the value prefix and suffix are left out because they never leave
/// the granted host.
public func proxyTouchIDReason(_ request: ProxyConfirmRequest) -> String {
    let target = request.target
    var facts = [
        "header \(oneLine(target.headerName, limit: 128))",
        target.methods.isEmpty
            ? "any method" : "methods \(oneLine(target.methods.joined(separator: " "), limit: 128))",
        target.pathGlob.isEmpty ? "any path" : "paths \(quoted(target.pathGlob))",
    ]
    if !target.placeholderEnv.isEmpty { facts.append("placeholder \(oneLine(target.placeholderEnv, limit: 64))") }
    return "let berth-proxy send secret \(request.secret) to \(oneLine(target.host, limit: 253)):\(target.port) "
        + "(\(facts.joined(separator: ", "))), requested by \(oneLine(request.requester.executable))"
}

public func confirmTouchIDReason(_ request: ConfirmRequest) -> String {
    let consumer = request.consumer
    var facts = ["sha256 \(consumer.sha256.prefix(12))"]
    if let signer = signer(consumer) { facts.insert(signer, at: 0) }
    facts.append(request.argsGlob.isEmpty ? "any arguments" : "arguments \(quoted(request.argsGlob))")
    if !request.passEnv.isEmpty {
        facts.append("also environment \(request.passEnv.map { oneLine($0, limit: 64) }.joined(separator: " "))")
    }
    let grant =
        "let \(oneLine(consumer.path)) (\(facts.joined(separator: ", "))) use secret \(request.secret), "
        + "requested by \(oneLine(request.requester.executable))"
    guard request.interpreter else { return grant }
    return "\(oneLine(consumer.path)) is an interpreter and can print this secret. " + grant
}

/// Names who signed the consumer from berthd's verified signature kind. Apple's platform binaries
/// have no team but are signed, so only code with no valid signature reads as unsigned.
func signer(_ consumer: Consumer) -> String? {
    switch consumer.signature {
    case "team": "team \(oneLine(consumer.teamID))"
    case "apple": "Apple (platform)"
    case "adhoc": "ad hoc"
    case "unsigned": "unsigned"
    case "other": "no team"
    default: consumer.teamID.isEmpty ? nil : "team \(oneLine(consumer.teamID))"
    }
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

/// The code requirement berth-native holds its parent to: the berth CLI signed by Berth's team
/// through Apple's chain, so a renamed or self signed copy cannot pass.
public let berthCLIRequirement =
    #"anchor apple generic and certificate leaf[subject.OU] = "XPJ7XA42J4" and identifier "com.wesleybldc.berth.cli""#

/// The same requirement for berth-proxy, the other parent read may hand a value to.
public let berthProxyRequirement =
    #"anchor apple generic and certificate leaf[subject.OU] = "XPJ7XA42J4" and identifier "com.wesleybldc.berth.proxy""#

/// The parents read accepts, and the consumer kind each must hold a ticket for.
public let parentRequirements: [(ConsumerKind, String)] = [
    (.executable, berthCLIRequirement), (.proxy, berthProxyRequirement),
]
