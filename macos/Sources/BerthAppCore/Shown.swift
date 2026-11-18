import Foundation

/// The Swift twin of the berth CLI's shown: text a process chose is shown as is when every
/// character is graphic, else quoted with Go's escapes (strconv.QuoteToGraphic), so a browser's
/// owner name or profile folder cannot hide text behind control, bidi or format characters.
/// The cases in Fixtures/shown.json come from the Go function itself.
public func shown(_ text: String) -> String {
    text.unicodeScalars.allSatisfy(isGraphic) ? text : quoted(text)
}

/// Text always quoted with Go's escapes (strconv.QuoteToGraphic), for where a person must see
/// where agent or process chosen text starts and ends, as in a notification.
public func quoted(_ text: String) -> String {
    var out = "\""
    for scalar in text.unicodeScalars {
        switch scalar {
        case "\"": out += "\\\""
        case "\\": out += "\\\\"
        case _ where isGraphic(scalar): out.unicodeScalars.append(scalar)
        case "\u{07}": out += "\\a"
        case "\u{08}": out += "\\b"
        case "\u{0C}": out += "\\f"
        case "\n": out += "\\n"
        case "\r": out += "\\r"
        case "\t": out += "\\t"
        case "\u{0B}": out += "\\v"
        case _ where scalar.value < 0x20 || scalar.value == 0x7F: out += "\\x" + hex(scalar.value, width: 2)
        case _ where scalar.value < 0x10000: out += "\\u" + hex(scalar.value, width: 4)
        default: out += "\\U" + hex(scalar.value, width: 8)
        }
    }
    return out + "\""
}

/// Go's unicode.IsGraphic: letters, marks, numbers, punctuation, symbols and the space separators.
func isGraphic(_ scalar: Unicode.Scalar) -> Bool {
    switch scalar.properties.generalCategory {
    case .control, .format, .surrogate, .privateUse, .unassigned, .lineSeparator, .paragraphSeparator:
        false
    default:
        true
    }
}

private func hex(_ value: UInt32, width: Int) -> String {
    let digits = String(value, radix: 16)
    return String(repeating: "0", count: max(0, width - digits.count)) + digits
}
