import Foundation
import Testing

@testable import BerthAppCore

private struct ShownCase: Decodable {
    var `in`: String
    var out: String
}

/// Every case was written by the Go test from the CLI's own shown.
@Test func shownMatchesTheCLI() throws {
    let cases = try JSONDecoder().decode([ShownCase].self, from: fixture("shown.json"))
    #expect(cases.count > 20)
    for c in cases {
        #expect(shown(c.in) == c.out, "input \(c.in.unicodeScalars.map { String($0.value, radix: 16) })")
    }
}

@Test func shownLeavesNoEscapeByte() throws {
    let cases = try JSONDecoder().decode([ShownCase].self, from: fixture("shown.json"))
    for c in cases { #expect(!shown(c.in).unicodeScalars.contains { $0.value == 0x1B }) }
}
