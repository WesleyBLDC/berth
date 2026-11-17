// Finds the largest on screen window of a process and captures it with ScreenCaptureKit.
import CoreGraphics
import Foundation
import ImageIO
import ScreenCaptureKit
import UniformTypeIdentifiers

guard CommandLine.arguments.count == 3, let pid = pid_t(CommandLine.arguments[1]) else {
    FileHandle.standardError.write(Data("usage: WindowProof <pid> <out.png>\n".utf8))
    exit(64)
}
let out = URL(fileURLWithPath: CommandLine.arguments[2])

do {
    let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
    let windows = content.windows.filter { $0.owningApplication?.processID == pid && $0.frame.width > 100 }
    guard let window = windows.max(by: { $0.frame.width * $0.frame.height < $1.frame.width * $1.frame.height }) else {
        print("no on screen window for pid \(pid) among \(content.windows.count) windows")
        exit(1)
    }
    let config = SCStreamConfiguration()
    config.width = Int(window.frame.width * 2)
    config.height = Int(window.frame.height * 2)
    config.showsCursor = false
    let filter = SCContentFilter(desktopIndependentWindow: window)
    let image: CGImage = try await withCheckedThrowingContinuation { continuation in
        SCScreenshotManager.captureImage(contentFilter: filter, configuration: config) { image, error in
            if let image {
                continuation.resume(returning: image)
            } else {
                continuation.resume(throwing: error ?? CocoaError(.featureUnsupported))
            }
        }
    }

    guard let dest = CGImageDestinationCreateWithURL(out as CFURL, UTType.png.identifier as CFString, 1, nil) else {
        exit(1)
    }
    CGImageDestinationAddImage(dest, image, nil)
    guard CGImageDestinationFinalize(dest) else { exit(1) }

    // A blank capture is a known failure mode without permission, so check the pixels vary.
    let data = image.dataProvider?.data as Data?
    let distinct = Set(
        (data ?? Data()).prefix(400_000).enumerated().compactMap { $0.offset % 97 == 0 ? $0.element : nil }
    ).count
    print(
        "captured window \"\(window.title ?? "")\" of \(window.owningApplication?.applicationName ?? "?") pid \(pid): \(image.width)x\(image.height), distinct sampled bytes \(distinct)"
    )
    exit(distinct > 8 ? 0 : 1)
} catch {
    print("capture failed: \(error.localizedDescription) (\((error as NSError).code))")
    exit(2)
}
