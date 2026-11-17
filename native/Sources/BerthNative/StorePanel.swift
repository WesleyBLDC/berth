import AppKit
import BerthNativeCore

// Colors from brand/colors/tokens.json. Mist on navy and secondary on navy pass WCAG AA by a
// wide margin (about 14:1 and 10:1); navy text on brass is about 9:1.
enum Brand {
    static let navy800 = rgb(0x16, 0x26, 0x3F)
    static let navy900 = rgb(0x0A, 0x14, 0x24)
    static let mist = rgb(0xF4, 0xF7, 0xFA)
    static let secondary = rgb(0xC9, 0xD2, 0xDC)
    static let brass = rgb(0xF2, 0xC4, 0x6D)

    static var icon: NSImage? { NSImage(data: Data(PackageResources.berth_icon_png)) }

    private static func rgb(_ r: Int, _ g: Int, _ b: Int) -> NSColor {
        NSColor(srgbRed: CGFloat(r) / 255, green: CGFloat(g) / 255, blue: CGFloat(b) / 255, alpha: 1)
    }
}

/// The app background: navy 800 at the top fading to navy 900 at the bottom.
final class GradientView: NSView {
    override func draw(_ dirtyRect: NSRect) {
        NSGradient(starting: Brand.navy800, ending: Brand.navy900)?.draw(in: bounds, angle: -90)
    }
}

/// Brass is the one accent on the screen, so only the confirm button carries it.
final class BrassButton: NSButton {
    override var wantsUpdateLayer: Bool { true }

    override func updateLayer() {
        layer?.backgroundColor = Brand.brass.cgColor
        layer?.cornerRadius = 6
    }
}

/// The store dialog: a branded panel instead of NSAlert, because NSAlert offers no way to
/// color its background or text. The title, body and button label come unchanged from
/// storeDialogText or rotateDialogText.
@MainActor
final class StorePanel: NSObject {
    let panel: NSPanel
    let field = NSSecureTextField()

    init(_ text: DialogText) {
        panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: 480, height: 200),
            styleMask: [.titled, .fullSizeContentView],
            backing: .buffered,
            defer: false
        )
        super.init()
        panel.titleVisibility = .hidden
        panel.titlebarAppearsTransparent = true
        // System controls (the secure field, Cancel) then draw for a dark background.
        panel.appearance = NSAppearance(named: .darkAqua)
        panel.backgroundColor = Brand.navy900

        let title = label(text.title, font: .boldSystemFont(ofSize: 14), color: Brand.mist)
        let body = label(text.body, font: .systemFont(ofSize: 12), color: Brand.secondary)
        // The system placeholder gray is too faint on navy, so it uses the secondary text color.
        field.placeholderAttributedString = NSAttributedString(
            string: "Paste the value",
            attributes: [.foregroundColor: Brand.secondary, .font: NSFont.systemFont(ofSize: 13)]
        )
        field.translatesAutoresizingMaskIntoConstraints = false
        field.widthAnchor.constraint(equalToConstant: 340).isActive = true

        let cancel = NSButton(title: "Cancel", target: self, action: #selector(cancelled))
        cancel.keyEquivalent = "\u{1b}"
        let save = BrassButton(title: "", target: self, action: #selector(saved))
        save.isBordered = false
        save.attributedTitle = NSAttributedString(
            string: text.button,
            attributes: [.foregroundColor: Brand.navy800, .font: NSFont.systemFont(ofSize: 13, weight: .semibold)]
        )
        save.keyEquivalent = "\r"
        save.translatesAutoresizingMaskIntoConstraints = false
        save.widthAnchor.constraint(greaterThanOrEqualToConstant: 84).isActive = true
        save.heightAnchor.constraint(equalToConstant: 28).isActive = true
        let buttons = NSStackView(views: [NSView(), cancel, save])
        buttons.spacing = 12

        let column = NSStackView(views: [title, body, field, buttons])
        column.orientation = .vertical
        column.alignment = .leading
        column.spacing = 12
        buttons.widthAnchor.constraint(equalTo: column.widthAnchor).isActive = true

        var row: [NSView] = []
        if let icon = Brand.icon {
            let image = NSImageView(image: icon)
            image.translatesAutoresizingMaskIntoConstraints = false
            image.widthAnchor.constraint(equalToConstant: 64).isActive = true
            image.heightAnchor.constraint(equalToConstant: 64).isActive = true
            row.append(image)
        }
        row.append(column)
        let content = NSStackView(views: row)
        content.alignment = .top
        content.spacing = 16
        content.translatesAutoresizingMaskIntoConstraints = false

        let background = GradientView()
        background.addSubview(content)
        NSLayoutConstraint.activate([
            content.topAnchor.constraint(equalTo: background.topAnchor, constant: 32),
            content.bottomAnchor.constraint(equalTo: background.bottomAnchor, constant: -20),
            content.leadingAnchor.constraint(equalTo: background.leadingAnchor, constant: 20),
            content.trailingAnchor.constraint(equalTo: background.trailingAnchor, constant: -20),
        ])
        panel.contentView = background
        panel.setContentSize(background.fittingSize)
        panel.initialFirstResponder = field
        panel.center()
    }

    /// Shows the panel modally; true when the person chose Save.
    func run() -> Bool {
        let response = NSApplication.shared.runModal(for: panel)
        panel.orderOut(nil)
        return response == .OK
    }

    @objc private func saved() { NSApplication.shared.stopModal(withCode: .OK) }
    @objc private func cancelled() { NSApplication.shared.stopModal(withCode: .cancel) }

    private func label(_ text: String, font: NSFont, color: NSColor) -> NSTextField {
        let l = NSTextField(wrappingLabelWithString: text)
        l.font = font
        l.textColor = color
        l.preferredMaxLayoutWidth = 340
        l.isSelectable = false
        return l
    }
}
