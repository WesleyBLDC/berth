import BerthAppCore
import ServiceManagement

/// The LaunchAgent in Contents/Library/LaunchAgents that runs Contents/Helpers/berthd. Only the
/// settings buttons call register and unregister; macOS then asks the person to allow it.
@MainActor
struct LaunchAgentLoginItem: LoginItemService {
    static let plistName = "com.wesleybldc.berth.daemon.plist"

    private let service = SMAppService.agent(plistName: Self.plistName)

    var status: LoginItemStatus {
        switch service.status {
        case .enabled: .enabled
        case .requiresApproval: .requiresApproval
        case .notRegistered: .notRegistered
        case .notFound: .notFound
        @unknown default: .notFound
        }
    }

    func register() throws { try service.register() }

    func unregister() throws { try service.unregister() }

    func openSettings() { SMAppService.openSystemSettingsLoginItems() }

    /// Contents/Helpers/berthd of the running bundle, which the LaunchAgent runs.
    static var bundledDaemon: String {
        Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/berthd").resolvingSymlinksInPath().path
    }
}
