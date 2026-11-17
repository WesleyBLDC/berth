import Foundation
import Testing

@testable import BerthNativeCore

let hash64 = String(repeating: "ab", count: 32)

let storeLine = #"""
    {"name":"github-token","reason":"open a PR","requester":{"pid":42,"executable":"/usr/local/bin/claude","tree":[{"pid":42,"executable":"/usr/local/bin/claude"},{"pid":7,"executable":"/bin/zsh"}],"working_dir":"/Users/me/code"}}

    """#

let confirmLine = """
    {"action":"grant","secret":"github-token","consumer":{"path":"/opt/homebrew/bin/gh","sha256":"\(hash64)",\
    "team_id":"ABCDE12345","signing_id":"com.github.gh","signature":"team"},"args_glob":"pr *","requester":{"pid":42,\
    "executable":"/usr/local/bin/claude","tree":[],"working_dir":"/tmp"}}
    """

@Test func decodesStoreRequest() throws {
    let request = try decodeStore(Data(storeLine.utf8))
    #expect(request.name == "github-token")
    #expect(request.reason == "open a PR")
    #expect(request.requester.pid == 42)
    #expect(request.requester.workingDir == "/Users/me/code")
    #expect(
        request.requester.tree == [
            TreeProcess(pid: 42, executable: "/usr/local/bin/claude"), TreeProcess(pid: 7, executable: "/bin/zsh"),
        ])
}

@Test func decodesConfirmRequest() throws {
    guard case .grant(let request) = try decodeConfirm(Data(confirmLine.utf8)) else {
        Issue.record("a grant line decoded as another action")
        return
    }
    #expect(request.secret == "github-token")
    #expect(
        request.consumer
            == Consumer(
                path: "/opt/homebrew/bin/gh", sha256: hash64, teamID: "ABCDE12345",
                signingID: "com.github.gh", signature: "team"))
    #expect(request.argsGlob == "pr *")
    #expect(request.requester.executable == "/usr/local/bin/claude")
}

@Test(arguments: ["", "{", "[]", #"{"name":"x"}"#, #"{"name":"x","reason":"r","requester":{"pid":"1"}}"#])
func rejectsMalformedStore(_ line: String) {
    #expect(throws: ProtocolError.badJSON) { try decodeStore(Data(line.utf8)) }
}

@Test(arguments: ["", "-x", ".x", "Upper", "a/b", "a b", "berth..x/../y", String(repeating: "a", count: 129)])
func rejectsBadNames(_ name: String) {
    #expect(!isValidSecretName(name))
    let line = storeLine.replacingOccurrences(of: #""github-token""#, with: #""\#(name)""#)
    #expect(throws: (any Error).self) { try decodeStore(Data(line.utf8)) }
}

@Test(arguments: ["a", "0", "github-token", "a.b_c-d", String(repeating: "a", count: 128)])
func acceptsGoodNames(_ name: String) {
    #expect(isValidSecretName(name))
    #expect(keychainService(for: name) == "berth.\(name)")
}

@Test func rejectsUnknownConfirmAction() {
    let line = confirmLine.replacingOccurrences(of: #""action":"grant""#, with: #""action":"read""#)
    #expect(throws: ProtocolError.unknownAction("read")) { try decodeConfirm(Data(line.utf8)) }
}

@Test(arguments: [
    "", String(repeating: "AB", count: 32), String(repeating: "a", count: 63), String(repeating: "g", count: 64),
])
func rejectsBadConsumerHash(_ hash: String) {
    let line = confirmLine.replacingOccurrences(of: hash64, with: hash)
    #expect(throws: ProtocolError.invalidSHA256) { try decodeConfirm(Data(line.utf8)) }
}

@Test func encodesRepliesAsOneJSONLine() {
    #expect(String(decoding: encodeReply(Reply(StoreOutcome.stored)), as: UTF8.self) == "{\"outcome\":\"stored\"}\n")
    #expect(
        String(decoding: encodeReply(Reply(ConfirmOutcome.approved)), as: UTF8.self) == "{\"outcome\":\"approved\"}\n")
    #expect(
        String(decoding: encodeReply(Reply(StoreOutcome.failed, error: "berth.x already exists")), as: UTF8.self)
            == "{\"error\":\"berth.x already exists\",\"outcome\":\"failed\"}\n")
    #expect(String(decoding: encodeReply(Reply(ConfirmOutcome.denied)), as: UTF8.self) == "{\"outcome\":\"denied\"}\n")
}

@Test func socketPathFollowsBerthHome() {
    #expect(
        socketPath(environment: ["BERTH_HOME": "/tmp/b", "HOME": "/Users/me"], fallbackHome: "/x")
            == "/tmp/b/berthd.sock")
    #expect(
        socketPath(environment: ["HOME": "/Users/me"], fallbackHome: "/x")
            == "/Users/me/Library/Application Support/Berth/berthd.sock")
    #expect(
        socketPath(environment: ["BERTH_HOME": ""], fallbackHome: "/x")
            == "/x/Library/Application Support/Berth/berthd.sock")
}

let proxyLine = """
    {"action":"proxy","secret":"openai","target":{"host":"api.openai.com","port":443,"header_name":"Authorization",\
    "value_prefix":"Bearer ","value_suffix":"","path_glob":"/v1/*","methods":["GET","POST"],\
    "placeholder_env":"OPENAI_API_KEY"},"requester":{"pid":42,"executable":"/usr/local/bin/claude","tree":[],\
    "working_dir":"/tmp"}}
    """

@Test func decodesProxyConfirmRequest() throws {
    guard case .proxy(let request) = try decodeConfirm(Data(proxyLine.utf8)) else {
        Issue.record("a proxy line decoded as another action")
        return
    }
    #expect(request.secret == "openai")
    #expect(
        request.target
            == ProxyTarget(
                host: "api.openai.com", port: 443, headerName: "Authorization", valuePrefix: "Bearer ",
                pathGlob: "/v1/*", methods: ["GET", "POST"], placeholderEnv: "OPENAI_API_KEY"))
}

@Test(arguments: [#""port":443"#, #""host":"api.openai.com""#, #""header_name":"Authorization""#])
func rejectsBadProxyTarget(_ field: String) {
    let broken = field.replacingOccurrences(of: "443", with: "0").replacingOccurrences(
        of: #"api.openai.com""#, with: #"""#
    )
    .replacingOccurrences(of: #"Authorization""#, with: #"""#)
    let line = proxyLine.replacingOccurrences(of: field, with: broken)
    #expect(throws: ProtocolError.invalidTarget) { try decodeConfirm(Data(line.utf8)) }
}

@Test func rejectsProxyConfirmWithoutTarget() {
    let line = #"{"action":"proxy","secret":"k","requester":{"pid":1,"executable":"/x","tree":[],"working_dir":"/"}}"#
    #expect(throws: ProtocolError.badJSON) { try decodeConfirm(Data(line.utf8)) }
}

@Test func encodesRemoveReplies() {
    #expect(String(decoding: encodeReply(Reply(RemoveOutcome.removed)), as: UTF8.self) == "{\"outcome\":\"removed\"}\n")
}
