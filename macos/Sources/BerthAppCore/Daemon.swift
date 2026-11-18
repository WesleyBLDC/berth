import BerthSocket
import Foundation

/// Where the app watches sessions from; the real one is DaemonClient, tests use a fake.
public protocol SessionSource: Sendable {
    /// Handshakes, then streams WatchSessions after the given event id (0 starts with a snapshot).
    func watch(after: Int64) -> AsyncThrowingStream<WatchSessionsResponse, any Error>
}

/// Where the settings read which berthd answers; the real one is DaemonClient.
public protocol StatusSource: Sendable {
    func status() async throws -> StatusResponse
}

/// The calls the app makes to berthd.
public struct DaemonClient: SessionSource, StatusSource {
    public var connect: ConnectClient
    public var clientName: String

    public init(connect: ConnectClient, clientName: String = "Berth.app") {
        self.connect = connect
        self.clientName = clientName
    }

    /// The client for berthd's socket under BERTH_HOME, trusting only the signed berthd.
    public static func standard(environment: [String: String] = ProcessInfo.processInfo.environment) -> DaemonClient {
        let path = socketPath(environment: environment, fallbackHome: NSHomeDirectory())
        return DaemonClient(connect: ConnectClient(dialer: SocketDialer(path: path)))
    }

    public func handshake() async throws -> HandshakeResponse {
        let response = try await connect.unary(
            "/berth.v1.DaemonService/Handshake",
            HandshakeRequest(protocolVersion: protocolVersion, clientName: clientName), as: HandshakeResponse.self)
        guard response.protocolVersion == protocolVersion else {
            throw ClientError.protocolMismatch(response.protocolVersion)
        }
        return response
    }

    public func status() async throws -> StatusResponse {
        _ = try await handshake()
        return try await connect.unary("/berth.v1.DaemonService/Status", StatusRequest(), as: StatusResponse.self)
    }

    public func watch(after: Int64) -> AsyncThrowingStream<WatchSessionsResponse, any Error> {
        let client = self
        return AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    _ = try await client.handshake()
                    let stream = client.connect.serverStream(
                        "/berth.v1.SessionsService/WatchSessions",
                        WatchSessionsRequest(afterEventId: ProtoInt64(after)), as: WatchSessionsResponse.self)
                    for try await message in stream { continuation.yield(message) }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }
}
