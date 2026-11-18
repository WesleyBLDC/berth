import AppKit
import BerthAppCore
import SwiftUI

@main
struct BerthApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @State private var model = SessionsModel(source: DaemonClient.standard())
    @State private var needs: NeedsModel
    @State private var notifications: NotificationSetting
    @State private var loginItem = LoginItemModel(
        service: LaunchAgentLoginItem(), source: DaemonClient.standard(),
        bundledDaemon: LaunchAgentLoginItem.bundledDaemon)

    init() {
        let needs = NeedsModel(source: DaemonClient.standard(), notifier: SystemNotifier.shared)
        _needs = State(initialValue: needs)
        _notifications = State(initialValue: NotificationSetting(needs: needs))
    }

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model, needs: needs)
        } label: {
            // The label lives as long as the app, so the watches run for the app's whole life.
            MenuLabel(count: model.store.counts.live)
                .task { await model.run() }
                .task { await needs.run() }
                .task { await notifications.restore() }
        }
        Window("Berth", id: "sessions") {
            TabView {
                Tab("Sessions", systemImage: "macwindow.on.rectangle") { SessionsWindow(model: model) }
                Tab("Settings", systemImage: "gearshape") {
                    SettingsSection(loginItem: loginItem, notifications: notifications)
                }
            }
        }
        .defaultSize(width: 980, height: 520)
    }
}

/// The brand's template image, which macOS tints, and the number of live sessions.
struct MenuLabel: View {
    var count: Int
    @Environment(\.openWindow) private var openWindow

    private static let icon: NSImage = {
        let image = NSImage(data: Data(PackageResources.berthTemplate_2x_png)) ?? NSImage()
        image.size = NSSize(width: 15, height: 18)
        image.isTemplate = true
        return image
    }()

    var body: some View {
        HStack(spacing: 4) {
            Image(nsImage: Self.icon)
            Text("\(count)").monospacedDigit()
        }
        .accessibilityLabel("Berth, \(count) browser sessions")
        .onAppear {
            SystemNotifier.shared.onOpen = {
                NSApp.activate()
                openWindow(id: "sessions")
            }
        }
    }
}

struct MenuContent: View {
    var model: SessionsModel
    var needs: NeedsModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        let counts = model.store.counts
        Text("\(counts.live) browsers: \(counts.active) active, \(counts.idle) idle")
        if counts.leftover > 0 {
            Text("\(counts.leftover) leftover, owner exited")
        }
        if !needs.store.open.isEmpty {
            Text("\(needs.store.open.count) waiting on you")
        }
        Text(model.link.text)
        Divider()
        Button("Open Berth") {
            NSApp.activate()
            openWindow(id: "sessions")
        }
        .keyboardShortcut("o")
        Divider()
        Button("Quit Berth") { NSApp.terminate(nil) }.keyboardShortcut("q")
    }
}
