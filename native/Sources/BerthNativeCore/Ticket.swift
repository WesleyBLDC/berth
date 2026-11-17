import Foundation

public enum TicketError: Error, Equatable, CustomStringConvertible {
    case malformed

    public var description: String { "ticket is not 64 lowercase hex characters and a newline" }
}

/// Parses the read command's stdin: exactly 64 lowercase hex characters, then one optional newline.
public func parseTicket(_ input: Data) throws(TicketError) -> [UInt8] {
    var chars = Array(input)
    if chars.last == UInt8(ascii: "\n") { chars.removeLast() }
    guard chars.count == 64, chars.allSatisfy(isLowerHex) else { throw .malformed }
    var ticket = [UInt8](repeating: 0, count: 32)
    for i in 0..<32 {
        ticket[i] = hexValue(chars[2 * i]) << 4 | hexValue(chars[2 * i + 1])
    }
    return ticket
}

private func hexValue(_ char: UInt8) -> UInt8 {
    char >= UInt8(ascii: "a") ? char - UInt8(ascii: "a") + 10 : char - UInt8(ascii: "0")
}

public func hexString(_ bytes: [UInt8]) -> String {
    bytes.map { String(format: "%02x", $0) }.joined()
}
