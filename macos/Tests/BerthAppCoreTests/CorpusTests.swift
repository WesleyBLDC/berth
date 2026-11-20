import CryptoKit
import Foundation
import Testing

@testable import BerthAppCore

/// The corpora are written by TestSwiftCorpus in internal/cli from Go's own shown, quoted and
/// connect-go's stream reader; every entry must come out of the app's twins the same.
private struct TextEntry: Decodable {
    var `in`: String
    var `repeat`: Int
    var valid: Bool
    var out: String
    var quoted: String
    var outSHA256: String?
    var quotedSHA256: String?
}

private struct EnvelopeEntry: Decodable {
    var name: String
    var body: String
    var cuts: [Int]
    var messages: [String]
    var outcome: String
}

private func bytes(hex: String) -> [UInt8] {
    let digits = Array(hex.utf8)
    return stride(from: 0, to: digits.count, by: 2).map { i in
        UInt8(String(decoding: digits[i..<i + 2], as: UTF8.self), radix: 16) ?? 0
    }
}

private func sha256(_ text: String) -> String {
    SHA256.hash(data: Data(text.utf8)).map { String(format: "%02x", $0) }.joined()
}

@Test func shownAndQuotedAgreeWithGoOnTheCorpus() throws {
    let entries = try JSONDecoder().decode([TextEntry].self, from: fixture("text_corpus.json"))
    #expect(entries.count > 400)
    for (i, e) in entries.enumerated() {
        let input = Array([[UInt8]](repeating: bytes(hex: e.in), count: e.repeat).joined())
        // A Swift String holds only valid UTF-8, so the app never sees what Go escapes as bytes.
        guard let text = String(validating: input, as: UTF8.self) else {
            #expect(!e.valid, "entry \(i) (\(e.in)) is UTF-8 to Go but not to Swift")
            continue
        }
        #expect(e.valid, "entry \(i) (\(e.in)) is UTF-8 to Swift but not to Go")
        if e.repeat == 1 {
            #expect(shown(text) == e.out, "shown, entry \(i) (\(e.in))")
            #expect(quoted(text) == e.quoted, "quoted, entry \(i) (\(e.in))")
        } else {
            #expect(sha256(shown(text)) == e.outSHA256, "shown, entry \(i) (\(e.in) x \(e.repeat))")
            #expect(sha256(quoted(text)) == e.quotedSHA256, "quoted, entry \(i) (\(e.in) x \(e.repeat))")
        }
    }
}

private func read(_ entry: EnvelopeEntry) async -> (messages: [String], outcome: String) {
    let head = Array("HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\nConnection: close\r\n\r\n".utf8)
    var body = bytes(hex: entry.body)[...]
    var pieces = [head]
    var cuts = entry.cuts[...]
    while !body.isEmpty {
        let n = min(body.count, cuts.first ?? body.count)
        pieces.append(Array(body.prefix(n)))
        body = body.dropFirst(n)
        if cuts.count > 1 { cuts = cuts.dropFirst() }
    }
    let stream = ConnectClient(dialer: FakeDialer(connection: FakeConnection(pieces))).serverStream(
        "/berth.v1.SessionsService/WatchSessions", WatchSessionsRequest(afterEventId: ProtoInt64(0)),
        as: WatchSessionsResponse.self)
    var messages: [String] = []
    do {
        for try await m in stream {
            switch m {
            case .snapshot(let s): messages.append("snapshot \(s.eventID)")
            case .change(let c): messages.append("change \(c.eventID)")
            case .unknown: messages.append("unknown")
            }
        }
        return (messages, "ok")
    } catch ClientError.refused(let code, let message, _) where message == "berth says no" {
        return (messages, "refused \(code)")
    } catch {
        return (messages, "malformed")
    }
}

@Test func streamReadingAgreesWithConnectGoOnTheCorpus() async throws {
    let entries = try JSONDecoder().decode([EnvelopeEntry].self, from: fixture("envelopes.json"))
    #expect(entries.count > 150)
    for e in entries {
        let got = await read(e)
        #expect(got.messages == e.messages, "\(e.name): messages")
        #expect(got.outcome == e.outcome, "\(e.name): outcome")
    }
}
