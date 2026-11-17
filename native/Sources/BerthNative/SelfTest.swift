#if DEBUG
    import AppKit
    import Foundation

    /// Debug builds only: waits on an async step the way store and confirm wait on Touch ID, so a
    /// test can prove the helper stays alive until it replies.
    @MainActor
    func runSelfTestWait() async -> Never {
        _ = NSApplication.shared
        NSApplication.shared.setActivationPolicy(.accessory)
        // An offscreen modal session that ends by itself, like the store panel closing.
        let panel = NSPanel(
            contentRect: NSRect(x: -10_000, y: -10_000, width: 10, height: 10), styleMask: [.borderless],
            backing: .buffered, defer: false)
        let stop = Timer(timeInterval: 0.05, repeats: false) { _ in
            MainActor.assumeIsolated { NSApplication.shared.stopModal() }
        }
        RunLoop.main.add(stop, forMode: .modalPanel)
        NSApplication.shared.runModal(for: panel)
        panel.orderOut(nil)
        // Touch ID delivers its result through the main run loop, so wait on the same path.
        await withCheckedContinuation { (done: CheckedContinuation<Void, Never>) in
            RunLoop.main.perform { done.resume() }
        }
        FileHandle.standardOutput.write(Data("{\"outcome\":\"waited\"}\n".utf8))
        exit(0)
    }
#endif
