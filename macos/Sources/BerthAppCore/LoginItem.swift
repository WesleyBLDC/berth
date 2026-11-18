import Foundation
import Observation

/// SMAppService.Status for the LaunchAgent that runs the bundled berthd.
public enum LoginItemStatus: Equatable, Sendable {
    case notRegistered
    case enabled
    case requiresApproval
    case notFound

    public var text: String {
        switch self {
        case .notRegistered: "off"
        case .enabled: "on: launchd starts berthd at login"
        case .requiresApproval: "waiting for you to allow Berth in System Settings, General, Login Items"
        case .notFound: "not found: run Berth from its bundle to register the login item"
        }
    }
}

/// The login item as the settings use it; the real one wraps SMAppService.agent in the app.
@MainActor
public protocol LoginItemService {
    var status: LoginItemStatus { get }
    func register() throws
    func unregister() throws
    func openSettings()
}

/// The login item section: it reads the status and which berthd answers, and registers or
/// unregisters only when the person presses a button, never on its own.
@MainActor @Observable
public final class LoginItemModel {
    public private(set) var status: LoginItemStatus
    /// The last register or unregister failure, escaped.
    public private(set) var failure: String?
    /// Which berthd serves the socket, escaped.
    public private(set) var daemon = "asking berthd"

    @ObservationIgnored private let service: any LoginItemService
    @ObservationIgnored private let source: any StatusSource
    @ObservationIgnored private let bundledDaemon: String

    /// bundledDaemon is the path of Contents/Helpers/berthd in the running bundle.
    public init(service: any LoginItemService, source: any StatusSource, bundledDaemon: String) {
        self.service = service
        self.source = source
        self.bundledDaemon = bundledDaemon
        status = service.status
    }

    public func refresh() async {
        status = service.status
        do {
            let answer = try await source.status()
            let which = answer.executable == bundledDaemon ? "the bundle's berthd" : "a berthd started by hand"
            daemon = "\(which), \(shown(answer.executable)), pid \(answer.pid), version \(shown(answer.daemonVersion))"
        } catch {
            daemon = "no answer: \(shown(String(describing: error)))"
        }
    }

    public func register() { act { try $0.register() } }

    public func unregister() { act { try $0.unregister() } }

    public func openSettings() { service.openSettings() }

    private func act(_ change: (any LoginItemService) throws -> Void) {
        do {
            try change(service)
            failure = nil
        } catch {
            failure = shown(String(describing: error))
        }
        status = service.status
    }
}
