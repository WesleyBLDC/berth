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
