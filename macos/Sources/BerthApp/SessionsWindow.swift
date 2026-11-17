import BerthAppCore
import SwiftUI

/// Every browser session, live from WatchSessions. Each process chosen string comes from the
/// core's text helpers, which escape it like the CLI does.
struct SessionsWindow: View {
    var model: SessionsModel
    @State private var showStopped = false

    private var rows: [Session] { model.store.live + (showStopped ? model.store.stopped : []) }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Table(rows) {
                TableColumn("State") { Text($0.state.label) }.width(min: 70, ideal: 80)
                TableColumn("Browser") { Text($0.browserText) }.width(min: 90, ideal: 140)
                TableColumn("Driver") { Text($0.driverText) }.width(min: 70, ideal: 90)
                TableColumn("Headless") { Text($0.headlessText) }.width(min: 60, ideal: 70)
                TableColumn("Owner") { Text($0.ownerText) }.width(min: 90, ideal: 160)
                TableColumn("Profile") { Text($0.profileText).truncationMode(.middle) }
                TableColumn("Started") { session in
                    if let started = session.browser?.startedAt {
                        Text(started, style: .relative)
                    }
                }
                .width(min: 70, ideal: 90)
                TableColumn("ID") { Text(shown($0.id)).monospaced() }.width(min: 120, ideal: 170)
            }
            HStack {
                Text(model.link.text).foregroundStyle(.secondary)
                Spacer()
                Toggle("Show stopped", isOn: $showStopped).toggleStyle(.checkbox)
            }
            .padding(8)
        }
    }
}
