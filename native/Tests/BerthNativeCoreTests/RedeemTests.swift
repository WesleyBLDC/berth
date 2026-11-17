import Foundation
import Testing

@testable import BerthNativeCore

let ticketHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

@Test func parsesTicketWithNewline() throws {
    #expect(try parseTicket(Data((ticketHex + "\n").utf8)) == Array(0..<32))
    #expect(try parseTicket(Data(ticketHex.utf8)) == Array(0..<32))
    #expect(hexString(Array(0..<32)) == ticketHex)
}

@Test(arguments: [
    "", "\n", String(ticketHex.dropLast()) + "\n", ticketHex + "00\n", ticketHex.uppercased() + "\n",
    ticketHex + "\n\n", " " + ticketHex, String(ticketHex.dropLast()) + "g\n", ticketHex + "\r\n",
])
func rejectsMalformedTicket(_ input: String) {
    #expect(throws: TicketError.malformed) { try parseTicket(Data(input.utf8)) }
}

@Test func buildsConnectUnaryJSONRequest() throws {
    let raw = String(decoding: redeemRequest(ticket: Array(0..<32), requestID: "abcdef0123456789"), as: UTF8.self)
    let parts = raw.components(separatedBy: "\r\n\r\n")
    #expect(parts.count == 2)
    let lines = parts[0].components(separatedBy: "\r\n")
    #expect(lines[0] == "POST /berth.v1.SecretsService/RedeemUseTicket HTTP/1.1")
    #expect(lines.contains("Content-Type: application/json"))
    #expect(lines.contains("Connection: close"))
    #expect(lines.contains("Content-Length: \(parts[1].utf8.count)"))
    let body = try #require(JSONSerialization.jsonObject(with: Data(parts[1].utf8)) as? [String: String])
    #expect(body == ["requestId": "abcdef0123456789", "ticket": Data(Array<UInt8>(0..<32)).base64EncodedString()])
}

func response(_ status: String, headers: [String] = [], body: String) -> Data {
    Data((["HTTP/1.1 \(status)"] + headers + ["", body]).joined(separator: "\r\n").utf8)
}

@Test func parsesContentLengthResponse() throws {
    let body = #"{"name":"github-token"}"#
    let parsed = try parseHTTPResponse(
        response(
            "200 OK", headers: ["Content-Type: application/json", "Content-Length: \(body.utf8.count)"], body: body))
    #expect(parsed.status == 200)
    #expect(parsed.headers["content-type"] == "application/json")
    #expect(try redeemedName(from: parsed) == "github-token")
}

@Test func parsesChunkedResponse() throws {
    let raw = response(
        "200 OK", headers: ["Transfer-Encoding: chunked"],
        body: "5\r\n{\"nam\r\n12\r\ne\":\"github-token\"}\r\n0\r\n\r\n")
    #expect(try redeemedName(from: parseHTTPResponse(raw)) == "github-token")
}

@Test func parsesResponseReadToEndWithoutLength() throws {
    #expect(try redeemedName(from: parseHTTPResponse(response("200 OK", body: #"{"name":"a"}"#))) == "a")
}

@Test func connectErrorBodyIsAFailure() throws {
    let body = #"{"code":"permission_denied","message":"peer not trusted"}"#
    let parsed = try parseHTTPResponse(
        response("403 Forbidden", headers: ["Content-Length: \(body.utf8.count)"], body: body))
    #expect(throws: RedeemError.connectError("permission_denied")) { try redeemedName(from: parsed) }
    let sneaky = try parseHTTPResponse(response("200 OK", body: #"{"code":"internal","name":"a"}"#))
    #expect(throws: RedeemError.connectError("internal")) { try redeemedName(from: sneaky) }
}

@Test func non200IsAFailure() throws {
    let parsed = try parseHTTPResponse(response("404 Not Found", body: "404 page not found\n"))
    #expect(throws: RedeemError.status(404)) { try redeemedName(from: parsed) }
    let withName = try parseHTTPResponse(response("500 Internal Server Error", body: #"{"name":"a"}"#))
    #expect(throws: RedeemError.status(500)) { try redeemedName(from: withName) }
}

@Test(arguments: [#"{}"#, #"{"name":7}"#, "not json", #"{"name":"../escape"}"#, #"{"name":""}"#])
func badResponseBodiesFail(_ body: String) throws {
    let parsed = try parseHTTPResponse(response("200 OK", body: body))
    #expect(throws: (any Error).self) { try redeemedName(from: parsed) }
}

@Test(arguments: [
    "", "HTTP/1.1 200 OK\r\n", "garbage\r\n\r\n", "HTTP/1.1 abc OK\r\n\r\n", "HTTP/1.1 200 OK\r\nno colon\r\n\r\n",
    "HTTP/1.1 200 OK\r\nContent-Length: 50\r\n\r\nshort", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n",
])
func rejectsMalformedHTTP(_ raw: String) {
    #expect(throws: RedeemError.malformedResponse) { try parseHTTPResponse(Data(raw.utf8)) }
}
