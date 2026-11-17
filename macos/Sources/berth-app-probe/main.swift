import BerthAppCore
import Foundation

// berth-app-probe <socket> <code requirement> <after event id>
// Runs the app's own client against a real berthd for the end to end tests: one WatchSessions
// stream, one line per message, so Go can check what the app would show. Never bundled.
setvbuf(stdout, nil, _IOLBF, 0)
let args = CommandLine.arguments
guard args.count == 4, let after = Int64(args[3]) else {
    FileHandle.standardError.write(Data("usage: berth-app-probe <socket> <requirement> <after event id>\n".utf8))
    exit(2)
}
let client = DaemonClient(connect: ConnectClient(dialer: SocketDialer(path: args[1], requirement: args[2])))
do {
    for try await message in client.watch(after: after) {
        switch message {
        case .snapshot(let s):
            print("snapshot \(s.eventID) \(s.reason.rawValue) \(s.sessions.count)")
            for session in s.sessions { print("session \(shown(session.id)) \(session.state.label)") }
        case .change(let c):
            print("change \(c.eventID) \(shown(c.event)) \(shown(c.session.id)) \(c.session.state.label)")
        case .unknown:
            print("unknown")
        }
    }
    print("end")
} catch {
    print("error \(shown(String(describing: error)))")
    exit(1)
}
