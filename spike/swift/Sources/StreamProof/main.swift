import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SpikeProto

let path =
    CommandLine.arguments.dropFirst().first
    ?? NSString(string: "~/Library/Application Support/Berth/spike.sock").expandingTildeInPath
let total: Int32 = 1000

try await withGRPCClient(
    transport: .http2NIOPosix(target: .unixDomainSocket(path: path), transportSecurity: .plaintext)
) { client in
    let spike = Spike_V1_SpikeService.Client(wrapping: client)

    var countRequest = Spike_V1_CountRequest()
    countRequest.count = total
    let started = Date()
    let received = try await spike.count(countRequest) { response in
        var n: Int32 = 0
        for try await message in response.messages {
            n += 1
            precondition(message.seq == n, "out of order at \(n)")
        }
        return n
    }
    print("server stream: \(received)/\(total) in \(Int(Date().timeIntervalSince(started) * 1000)) ms")

    let inputStarted = Date()
    let summary = try await spike.sendInput { writer in
        for i in 1...total {
            var event = Spike_V1_SendInputRequest()
            event.seq = i
            event.kind = "mousemove"
            event.x = Double(i)
            event.y = Double(i)
            try await writer.write(event)
        }
    }
    print("client stream: \(summary.received)/\(total) in \(Int(Date().timeIntervalSince(inputStarted) * 1000)) ms")
    if received != total || summary.received != total { exit(1) }
}
