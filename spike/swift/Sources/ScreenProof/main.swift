import AppKit
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SpikeProto
import SwiftUI

@MainActor
final class LiveView: ObservableObject {
    @Published var image: NSImage?
    var received = 0
    var rendered = 0
    var latencies: [Double] = []
    var firstFrame: Date?

    func show(_ frame: Spike_V1_WatchScreenResponse) {
        received += 1
        guard let image = NSImage(data: frame.jpeg) else { return }
        self.image = image
        rendered += 1
        if firstFrame == nil { firstFrame = Date() }
        latencies.append(Double(Int64(Date().timeIntervalSince1970 * 1_000_000) - frame.sentUnixMicros) / 1000)
    }

    func report() {
        let seconds = firstFrame.map { Date().timeIntervalSince($0) } ?? 0
        let sorted = latencies.sorted()
        let p50 = sorted.isEmpty ? 0 : sorted[sorted.count / 2]
        let p95 = sorted.isEmpty ? 0 : sorted[Int(Double(sorted.count) * 0.95)]
        let size = image.map { "\(Int($0.size.width))x\(Int($0.size.height))" } ?? "none"
        print(String(format: "frames shown: %d in %.1f s = %.1f fps, size %@, latency p50 %.1f ms p95 %.1f ms",
                     rendered, seconds, Double(rendered) / max(seconds, 0.001), size, p50, p95))
        exit(rendered > 0 && Double(rendered) / max(seconds, 0.001) >= 15 ? 0 : 1)
    }
}

struct LiveViewWindow: View {
    @ObservedObject var model: LiveView
    var body: some View {
        Group {
            if let image = model.image {
                Image(nsImage: image).resizable().aspectRatio(contentMode: .fit)
            } else {
                Text("Waiting for frames").foregroundStyle(.white)
            }
        }
        .frame(minWidth: 960, minHeight: 600)
        .background(Color.black)
    }
}

let socket = CommandLine.arguments.dropFirst().first
    ?? NSString(string: "~/Library/Application Support/Berth/spike.sock").expandingTildeInPath
let model = LiveView()
let app = NSApplication.shared
app.setActivationPolicy(.regular)
let window = NSWindow(contentRect: NSRect(x: 200, y: 200, width: 960, height: 600),
                      styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
window.title = "Berth spike: live view"
window.contentView = NSHostingView(rootView: LiveViewWindow(model: model))
window.makeKeyAndOrderFront(nil)
app.activate()

Task.detached {
    try await withGRPCClient(
        transport: .http2NIOPosix(target: .unixDomainSocket(path: socket), transportSecurity: .plaintext)
    ) { client in
        let spike = Spike_V1_SpikeService.Client(wrapping: client)
        var request = Spike_V1_WatchScreenRequest()
        request.maxWidth = 1440
        let deadline = Date().addingTimeInterval(10)
        try await spike.watchScreen(request) { response in
            for try await frame in response.messages {
                await model.show(frame)
                if Date() > deadline { break }
            }
        }
    }
    await model.report()
}
app.run()
