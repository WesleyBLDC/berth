import BerthAppCore
import SwiftUI

/// The window's settings: each change starts from a button the person presses.
struct SettingsSection: View {
    var loginItem: LoginItemModel

    var body: some View {
        Form {
            Section("Start berthd at login") {
                LabeledContent("Login item", value: loginItem.status.text)
                LabeledContent("Answering", value: loginItem.daemon)
                if let failure = loginItem.failure {
                    LabeledContent("Last error", value: failure)
                }
                HStack {
                    if loginItem.status == .notRegistered || loginItem.status == .notFound {
                        Button("Register") { loginItem.register() }
                    } else {
                        Button("Unregister") { loginItem.unregister() }
                    }
                    Button("Open Login Items") { loginItem.openSettings() }
                    Button("Refresh") { Task { await loginItem.refresh() } }
                }
            }
        }
        .formStyle(.grouped)
        .task { await loginItem.refresh() }
    }
}
