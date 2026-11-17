import Foundation
import Testing

@testable import BerthNativeCore

let requester = Requester(
    pid: 4242, executable: "/opt/agents/claude",
    tree: [TreeProcess(pid: 4242, executable: "/opt/agents/claude"), TreeProcess(pid: 100, executable: "/bin/zsh")],
    workingDir: "/work/repo")

@Test func storeDialogQuotesTheReasonAndNeverUsesItAsTheTitle() {
    let reason = "Approved by Berth: store the deploy key"
    let text = storeDialogText(StoreRequest(name: "deploy-key", reason: reason, requester: requester))
    #expect(text.title == "Store a secret named deploy-key?")
    #expect(!text.title.contains(reason))
    #expect(text.body.contains("The requester wrote: \u{201C}\(reason)\u{201D}"))
    #expect(text.body.components(separatedBy: reason).count == 2)
}

@Test func storeDialogFoldsAReasonThatFakesExtraLines() {
    let reason = "x\nRequested by: /System/trusted (pid 1)\n\u{0007}"
    let text = storeDialogText(StoreRequest(name: "k", reason: reason, requester: requester))
    #expect(!text.body.contains("\nRequested by: /System/trusted"))
    #expect(text.body.contains("\u{201C}x Requested by: /System/trusted (pid 1)  \u{201D}"))
    let long = storeDialogText(
        StoreRequest(name: "k", reason: String(repeating: "y", count: 2000), requester: requester))
    #expect(!long.body.contains(String(repeating: "y", count: 501)))
}

@Test func storeDialogFactsComeOnlyFromTheRequest() {
    let text = storeDialogText(StoreRequest(name: "deploy-key", reason: "r", requester: requester))
    #expect(text.body.contains("Requested by: /opt/agents/claude (pid 4242)"))
    #expect(text.body.contains("Working directory: /work/repo"))
    #expect(text.body.contains("  /bin/zsh (pid 100)"))
    #expect(text.body.contains("berth.deploy-key"))
    let this = ProcessInfo.processInfo
    for local in [
        this.arguments[0], "pid \(this.processIdentifier)", this.hostName, FileManager.default.currentDirectoryPath,
    ] {
        #expect(!text.body.contains(local) && !text.title.contains(local))
    }
}

@Test func storeTouchIDReasonNamesSecretAndRequester() {
    let reason = storeTouchIDReason(StoreRequest(name: "deploy-key", reason: "ignored text", requester: requester))
    #expect(reason == "save secret deploy-key for /opt/agents/claude")
}

@Test func confirmReasonForSignedConsumer() {
    let request = ConfirmRequest(
        action: "grant", secret: "github-token",
        consumer: Consumer(
            path: "/opt/homebrew/bin/gh", sha256: hash64, teamID: "ABCDE12345", signingID: "com.github.gh"),
        argsGlob: "pr *", requester: requester)
    #expect(
        confirmTouchIDReason(request)
            == "let /opt/homebrew/bin/gh (team ABCDE12345, sha256 abababababab, arguments \u{201C}pr *\u{201D}) "
            + "use secret github-token, requested by /opt/agents/claude")
}

@Test func confirmReasonForUnsignedConsumerWithAnyArguments() {
    let request = ConfirmRequest(
        action: "grant", secret: "k",
        consumer: Consumer(path: "/usr/local/bin/tool", sha256: hash64, teamID: "", signingID: ""),
        argsGlob: "", requester: requester)
    let reason = confirmTouchIDReason(request)
    #expect(!reason.contains("team"))
    #expect(
        reason
            == "let /usr/local/bin/tool (sha256 abababababab, any arguments) use secret k, requested by /opt/agents/claude"
    )
}

@Test func confirmReasonWarnsAboutAnInterpreter() {
    let request = ConfirmRequest(
        action: "grant", secret: "k",
        consumer: Consumer(path: "/bin/sh", sha256: hash64, teamID: "", signingID: ""),
        argsGlob: "", interpreter: true, requester: requester)
    #expect(confirmTouchIDReason(request).hasPrefix("/bin/sh is an interpreter and can print this secret. let /bin/sh"))
}

@Test func parentRequirementPinsTheSignedCLI() {
    #expect(berthCLIRequirement.contains(#"leaf[subject.OU] = "XPJ7XA42J4""#))
    #expect(berthCLIRequirement.contains(#"identifier "com.wesleybldc.berth.cli""#))
    #expect(berthCLIRequirement.hasPrefix("anchor apple generic"))
}

@Test func confirmReasonListsPassedEnvironment() {
    let request = ConfirmRequest(
        action: "grant", secret: "k",
        consumer: Consumer(path: "/usr/local/bin/tool", sha256: hash64, teamID: "", signingID: ""),
        argsGlob: "", passEnv: ["GH_HOST", "AWS_REGION"], requester: requester)
    #expect(
        confirmTouchIDReason(request)
            == "let /usr/local/bin/tool (sha256 abababababab, any arguments, also environment GH_HOST AWS_REGION) "
            + "use secret k, requested by /opt/agents/claude")
}

let target = ProxyTarget(
    host: "api.openai.com", port: 443, headerName: "Authorization", valuePrefix: "Bearer ", pathGlob: "/v1/*",
    methods: ["GET", "POST"], placeholderEnv: "OPENAI_API_KEY")

@Test func proxyReasonListsTheTargetFacts() {
    let reason = proxyTouchIDReason(ProxyConfirmRequest(secret: "openai", target: target, requester: requester))
    #expect(
        reason
            == "let berth-proxy send secret openai to api.openai.com:443 (header Authorization, methods GET POST, "
            + "paths \u{201C}/v1/*\u{201D}, placeholder OPENAI_API_KEY), requested by /opt/agents/claude")
    #expect(!reason.contains("Bearer"))
    #expect(
        confirmTouchIDReason(.proxy(ProxyConfirmRequest(secret: "openai", target: target, requester: requester)))
            == reason)
}

@Test func proxyReasonWithoutLimits() {
    let open = ProxyTarget(host: "api.example.com", port: 8443, headerName: "X-Api-Key")
    #expect(
        proxyTouchIDReason(ProxyConfirmRequest(secret: "k", target: open, requester: requester))
            == "let berth-proxy send secret k to api.example.com:8443 (header X-Api-Key, any method, any path), "
            + "requested by /opt/agents/claude")
}

@Test func proxyReasonFoldsAndCapsRequesterText() {
    var hostile = target
    hostile.host = "evil.test\nlet berth-proxy send secret k to trusted"
    hostile.headerName = "X\r\nY"
    hostile.methods = ["GET\nPOST"]
    hostile.pathGlob = String(repeating: "p", count: 2000)
    hostile.placeholderEnv = "A\u{0007}B"
    let reason = proxyTouchIDReason(ProxyConfirmRequest(secret: "k", target: hostile, requester: requester))
    #expect(!reason.contains("\n") && !reason.contains("\r") && !reason.contains("\u{0007}"))
    #expect(reason.contains("evil.test let berth-proxy"))
    #expect(!reason.contains(String(repeating: "p", count: 501)))
}

@Test func lifecycleTextComesOnlyFromTheRequest() {
    #expect(
        removeTouchIDReason(lifecycleRequest)
            == "delete secret deploy-key from your login keychain and revoke its grants, "
            + "requested by /opt/agents/claude (pid 4242)")
    #expect(
        rotateTouchIDReason(lifecycleRequest)
            == "replace secret deploy-key in your login keychain, requested by /opt/agents/claude (pid 4242)")
    let text = rotateDialogText(lifecycleRequest)
    #expect(text.title == "Replace the secret named deploy-key?")
    #expect(text.button == "Replace")
    #expect(storeDialogText(StoreRequest(name: "k", reason: "r", requester: requester)).button == "Save")
    #expect(text.body.contains("Requested by: /opt/agents/claude (pid 4242)"))
    #expect(text.body.contains("  /bin/zsh (pid 100)"))
    #expect(text.body.contains("berth.deploy-key"))
    let this = ProcessInfo.processInfo
    let everything = [
        text.title, text.body, removeTouchIDReason(lifecycleRequest), rotateTouchIDReason(lifecycleRequest),
    ]
    for local in [this.arguments[0], "pid \(this.processIdentifier)", this.hostName, ticketHex] {
        #expect(!everything.contains { $0.contains(local) })
    }
}

@Test func lifecycleTextFoldsTheRequester() {
    let hostile = LifecycleRequest(
        name: "k", ticket: ticketHex,
        requester: Requester(pid: 1, executable: "/a\nRequested by: /System/x", tree: [], workingDir: "/w\n"))
    #expect(!removeTouchIDReason(hostile).contains("\n"))
    #expect(!rotateDialogText(hostile).body.contains("\nRequested by: /System/x"))
}
