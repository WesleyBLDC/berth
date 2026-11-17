// berth-native: the only Berth process that touches secret values. The protocol is in
// docs/plans/e2-terminal-secrets.md, "Native helper protocol".
import BerthNativeCore
import Foundation

enum Exit {
    static let misuse: Int32 = 2
    static let readFailed: Int32 = 3
}

func diagnose(_ message: String) {
    FileHandle.standardError.write(Data("berth-native: \(message)\n".utf8))
}

func misuse(_ message: String) -> Never {
    diagnose(message)
    exit(Exit.misuse)
}

func reply<Outcome: Codable & Sendable>(_ outcome: Outcome, _ error: String? = nil) -> Never {
    if let error { diagnose(error) }
    FileHandle.standardOutput.write(encodeReply(Reply(outcome, error: error)))
    exit(0)
}

// A caller that closes the pipe early must get an error from write, not kill the helper mid reply.
signal(SIGPIPE, SIG_IGN)

let arguments = CommandLine.arguments.dropFirst()
guard arguments.count == 1, let command = arguments.first else {
    misuse("usage: berth-native store | confirm | read | rotate | remove (one request on stdin)")
}
var commands = ["store", "confirm", "read", "rotate", "remove"]
#if DEBUG
    commands.append("selftest-wait")
#endif
guard commands.contains(command) else { misuse("unknown command \(command)") }
// One line, not end of file: the caller may keep stdin open while it waits for the reply.
let line = readLine(strippingNewline: false).map { Data($0.utf8) } ?? Data()

// Every command ends in exit with its reply. Run it as a task and serve the main queue forever:
// with top level await, the main run loop returns as soon as a dialog's modal session has ended
// and the command waits on Touch ID, and the process exits with no reply.
Task { @MainActor in
    switch command {
    case "store": await runStore(line)
    case "confirm": await runConfirm(line)
    case "read": runRead(line)
    case "rotate": await runRotate(line)
    case "remove": await runRemove(line)
    #if DEBUG
        case "selftest-wait": await runSelfTestWait()
    #endif
    default: misuse("unknown command \(command)")
    }
}
dispatchMain()
