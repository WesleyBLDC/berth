import AppKit
import BerthAppCore
import Observation
import UserNotifications

/// Posts one notification per need through UNUserNotificationCenter, identified by the need's
/// id so a resolved need's notification can be removed. Clicking one opens the window.
@MainActor
final class SystemNotifier: NSObject, Notifier, UNUserNotificationCenterDelegate {
    static let shared = SystemNotifier()

    /// Opens the window; the menu bar label sets it once SwiftUI can open windows.
    var onOpen: (() -> Void)?

    private var center: UNUserNotificationCenter { .current() }

    func post(id: String, text: NeedText) {
        let content = UNMutableNotificationContent()
        content.title = text.title
        content.body = text.body
        center.add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    func remove(id: String) {
        center.removeDeliveredNotifications(withIdentifiers: [id])
        center.removePendingNotificationRequests(withIdentifiers: [id])
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse
    ) async {
        await MainActor.run { onOpen?() }
    }
}

/// The notifications switch. macOS asks the person only when they press Turn On for the first
/// time, never at launch; reading the current permission asks nothing.
@MainActor @Observable
final class NotificationSetting {
    private(set) var text = "off"

    @ObservationIgnored private let needs: NeedsModel
    @ObservationIgnored private let defaults = UserDefaults.standard
    private static let offKey = "notificationsTurnedOff"

    init(needs: NeedsModel) { self.needs = needs }

    var isOn: Bool { needs.posting }

    /// At launch: posting resumes only when the person allowed it before and did not turn it off.
    func restore() async {
        let status = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
        if status == .authorized && !defaults.bool(forKey: Self.offKey) {
            needs.setPosting(true)
        }
        describe(status)
    }

    func turnOn() async {
        defaults.set(false, forKey: Self.offKey)
        let granted =
            (try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound])) ?? false
        needs.setPosting(granted)
        describe(await UNUserNotificationCenter.current().notificationSettings().authorizationStatus)
    }

    func turnOff() {
        defaults.set(true, forKey: Self.offKey)
        needs.setPosting(false)
        text = "off"
    }

    private func describe(_ status: UNAuthorizationStatus) {
        switch status {
        case .denied: text = "not allowed: turn on notifications for Berth in System Settings, Notifications"
        case _ where needs.posting: text = "on: one notification per thing that waits on you"
        default: text = "off"
        }
    }
}

/// Points notification clicks at SystemNotifier before the first one can arrive.
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated { UNUserNotificationCenter.current().delegate = SystemNotifier.shared }
    }
}
