import AppKit
import ImageIO
import SwiftUI
import Testing
import UniformTypeIdentifiers

/// How a view is drawn: a fixed size at scale 1, a fixed locale, calendar and time zone, no
/// animations, and the light or dark system appearance. Fonts are the system's, on every Mac.
enum Look: String, CaseIterable, Sendable {
    case light
    case dark

    var appearance: NSAppearance.Name { self == .dark ? .darkAqua : .aqua }
}

/// Snapshots/<name>-<look>.png beside this file. BERTH_SNAPSHOT_RECORD=1 rewrites them.
enum Snapshot {
    /// A channel this far apart still counts as the same pixel: anti aliasing and font
    /// rasterising differ slightly between machines and macOS updates.
    static let channelTolerance = 16
    /// At most this many pixels may differ beyond the channel tolerance. Renders are byte
    /// identical run to run on one Mac; a share of the image would be too loose, since one
    /// changed digit in the session list moves only about 25 pixels.
    static let differingMax = 10

    static let folder = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("Snapshots")
    static let failures = FileManager.default.temporaryDirectory.appendingPathComponent("berth-snapshots")
    static var recording: Bool { ProcessInfo.processInfo.environment["BERTH_SNAPSHOT_RECORD"] == "1" }

    /// Renders the view offscreen and compares it with its reference image, or records it.
    @MainActor
    static func check<V: View>(
        _ view: V, named name: String, look: Look, size: CGSize, sourceLocation: SourceLocation = #_sourceLocation
    ) throws {
        let actual = try render(view, look: look, size: size)
        let file = "\(name)-\(look.rawValue).png"
        let reference = folder.appendingPathComponent(file)
        if recording {
            try png(actual).write(to: reference)
            return
        }
        guard let data = try? Data(contentsOf: reference), let expected = Pixels(png: data) else {
            let saved = try keep(actual, as: file)
            let message =
                "no reference image \(reference.path); the actual one is \(saved.path); "
                + "run BERTH_SNAPSHOT_RECORD=1 make test-app to record it"
            Issue.record(Comment(rawValue: message), sourceLocation: sourceLocation)
            return
        }
        let differing = actual.differing(from: expected, tolerance: channelTolerance)
        guard actual.width != expected.width || actual.height != expected.height || differing.count > differingMax
        else { return }
        let saved = try keep(actual, as: file)
        let diff = try keep(actual.marking(differing), as: file.replacingOccurrences(of: ".png", with: "-diff.png"))
        let sizes = "\(actual.width)x\(actual.height) against \(expected.width)x\(expected.height)"
        let message: String =
            "\(file) differs from its reference: \(differing.count) pixels (at most \(differingMax)), \(sizes); "
            + "actual \(saved.path), diff \(diff.path); if the change is intended, "
            + "run BERTH_SNAPSHOT_RECORD=1 make test-app"
        Issue.record(Comment(rawValue: message), sourceLocation: sourceLocation)
    }

    /// Draws the view in a borderless window that is never shown, so nothing reaches the screen
    /// and the app asks for nothing. AppKit backed controls (tables, forms, buttons) need a real
    /// NSHostingView, which ImageRenderer would draw as placeholders.
    @MainActor
    static func render<V: View>(_ view: V, look: Look, size: CGSize) throws -> Pixels {
        NSApplication.shared.setActivationPolicy(.prohibited)
        let root =
            view
            .environment(\.locale, Locale(identifier: "en_US_POSIX"))
            .environment(\.calendar, Calendar(identifier: .gregorian))
            .environment(\.timeZone, .gmt)
            .tint(.blue)
            .transaction { $0.disablesAnimations = true }
            .frame(width: size.width, height: size.height)
            // A view that draws no background of its own sits on the window's, as in the app.
            .background(Color(nsColor: .windowBackgroundColor))
        let host = NSHostingView(rootView: root)
        host.frame = CGRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: host.frame, styleMask: [.borderless], backing: .buffered, defer: true)
        window.isReleasedWhenClosed = false
        window.appearance = NSAppearance(named: look.appearance)
        window.contentView = host
        defer { window.close() }
        NSAnimationContext.runAnimationGroup { context in
            context.duration = 0
            context.allowsImplicitAnimation = false
            host.layoutSubtreeIfNeeded()
        }
        // Tables fill their rows on the next turn of the run loop.
        RunLoop.main.run(until: Date(timeIntervalSinceNow: 0.05))
        host.layoutSubtreeIfNeeded()
        guard
            let rep = NSBitmapImageRep(
                bitmapDataPlanes: nil, pixelsWide: Int(size.width), pixelsHigh: Int(size.height), bitsPerSample: 8,
                samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0,
                bitsPerPixel: 0)
        else { throw SnapshotError.render("no bitmap") }
        rep.size = size
        NSAppearance(named: look.appearance)?.performAsCurrentDrawingAppearance {
            host.cacheDisplay(in: host.bounds, to: rep)
        }
        guard !window.isVisible else { throw SnapshotError.render("the window reached the screen") }
        guard let image = rep.cgImage, let pixels = Pixels(image) else { throw SnapshotError.render("no image") }
        return pixels
    }

    private static func keep(_ pixels: Pixels, as file: String) throws -> URL {
        try FileManager.default.createDirectory(at: failures, withIntermediateDirectories: true)
        let url = failures.appendingPathComponent(file)
        try png(pixels).write(to: url)
        return url
    }

    static func png(_ pixels: Pixels) throws -> Data {
        let data = NSMutableData()
        guard let image = pixels.image,
            let destination = CGImageDestinationCreateWithData(data, UTType.png.identifier as CFString, 1, nil)
        else { throw SnapshotError.render("no PNG encoder") }
        CGImageDestinationAddImage(destination, image, nil)
        guard CGImageDestinationFinalize(destination) else { throw SnapshotError.render("PNG encoding failed") }
        return data as Data
    }
}

enum SnapshotError: Error {
    case render(String)
}

/// An image as opaque 8 bit sRGB RGBA bytes, so a rendered image and a decoded PNG compare alike.
struct Pixels {
    let width: Int
    let height: Int
    var bytes: [UInt8]

    init?(_ image: CGImage) {
        width = image.width
        height = image.height
        bytes = [UInt8](repeating: 0, count: width * height * 4)
        guard let space = CGColorSpace(name: CGColorSpace.sRGB) else { return nil }
        let drawn = bytes.withUnsafeMutableBytes { buffer in
            guard
                let context = CGContext(
                    data: buffer.baseAddress, width: width, height: height, bitsPerComponent: 8, bytesPerRow: width * 4,
                    space: space, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
            else { return false }
            context.draw(image, in: CGRect(x: 0, y: 0, width: width, height: height))
            return true
        }
        guard drawn else { return nil }
    }

    init?(png: Data) {
        guard let source = CGImageSourceCreateWithData(png as CFData, nil),
            let image = CGImageSourceCreateImageAtIndex(source, 0, nil)
        else { return nil }
        self.init(image)
    }

    var image: CGImage? {
        guard let space = CGColorSpace(name: CGColorSpace.sRGB),
            let provider = CGDataProvider(data: Data(bytes) as CFData)
        else { return nil }
        return CGImage(
            width: width, height: height, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: width * 4, space: space,
            bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipLast.rawValue), provider: provider,
            decode: nil, shouldInterpolate: false, intent: .defaultIntent)
    }

    /// Indexes of the pixels whose red, green or blue differ by more than the tolerance; every
    /// pixel when the sizes differ.
    func differing(from other: Pixels, tolerance: Int) -> [Int] {
        guard width == other.width, height == other.height else { return Array(0..<(width * height)) }
        return (0..<(width * height)).filter { pixel in
            (0..<3).contains { abs(Int(bytes[pixel * 4 + $0]) - Int(other.bytes[pixel * 4 + $0])) > tolerance }
        }
    }

    /// The image faded, with the given pixels in red.
    func marking(_ pixels: [Int]) -> Pixels {
        var out = self
        for index in out.bytes.indices where index % 4 != 3 { out.bytes[index] = out.bytes[index] / 4 + 160 }
        for pixel in pixels where pixel < width * height {
            out.bytes[pixel * 4] = 255
            out.bytes[pixel * 4 + 1] = 0
            out.bytes[pixel * 4 + 2] = 0
        }
        return out
    }

    /// Mean relative luminance, 0 black to 1 white, of the pixels in a band of rows.
    func luminance(rows: Range<Int>? = nil) -> Double {
        let band = rows ?? 0..<height
        var sum = 0.0
        for y in band {
            for x in 0..<width {
                let at = (y * width + x) * 4
                sum += 0.2126 * Double(bytes[at]) + 0.7152 * Double(bytes[at + 1]) + 0.0722 * Double(bytes[at + 2])
            }
        }
        return sum / 255 / Double(max(band.count * width, 1))
    }
}
