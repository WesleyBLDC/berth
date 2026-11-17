import Darwin
import Foundation
import Security

/// The code requirement berthd must meet before berth-native sends it a ticket, so a server an
/// agent started on some socket learns nothing and the read fails closed.
public let berthDaemonRequirement =
    #"anchor apple generic and certificate leaf[subject.OU] = "XPJ7XA42J4" and identifier "com.wesleybldc.berth.daemon""#

public struct SocketError: Error, Equatable, CustomStringConvertible {
    public var description: String
}

/// A connected Unix socket held as a plain file descriptor, so the caller can ask the kernel
/// who is on the other end before writing anything.
public final class UnixSocket {
    public let fd: Int32

    public init(path: String, timeout: TimeInterval = 10) throws(SocketError) {
        var address = sockaddr_un()
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        guard path.utf8.count < capacity else { throw SocketError(description: "socket path too long: \(path)") }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw SocketError(description: "socket: \(String(cString: strerror(errno)))") }
        self.fd = fd
        var limit = timeval(tv_sec: Int(timeout), tv_usec: 0)
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &limit, socklen_t(MemoryLayout<timeval>.size))
        _ = setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &limit, socklen_t(MemoryLayout<timeval>.size))
        var noSigPipe: Int32 = 1
        _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        address.sun_family = sa_family_t(AF_UNIX)
        withUnsafeMutableBytes(of: &address.sun_path) { raw in
            raw.copyBytes(from: path.utf8)
            raw[path.utf8.count] = 0
        }
        let connected = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connected == 0 else {
            let reason = String(cString: strerror(errno))
            close(fd)
            throw SocketError(description: "connect \(path): \(reason)")
        }
    }

    deinit { close(fd) }

    /// The audit token of the process that answers, from the kernel (LOCAL_PEERTOKEN).
    public func peerAuditToken() throws(SocketError) -> audit_token_t {
        var token = audit_token_t()
        var size = socklen_t(MemoryLayout<audit_token_t>.size)
        guard getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &size) == 0,
            size == socklen_t(MemoryLayout<audit_token_t>.size)
        else { throw SocketError(description: "LOCAL_PEERTOKEN: \(String(cString: strerror(errno)))") }
        return token
    }

    public func send(_ data: Data) throws(SocketError) {
        let ok = data.withUnsafeBytes { raw -> Bool in
            guard var next = raw.baseAddress else { return true }
            var left = raw.count
            while left > 0 {
                let n = write(fd, next, left)
                if n < 0 {
                    if errno == EINTR { continue }
                    return false
                }
                next += n
                left -= n
            }
            return true
        }
        guard ok else { throw SocketError(description: "write: \(String(cString: strerror(errno)))") }
    }

    /// Reads until the peer closes, refusing more than limit bytes.
    public func readAll(limit: Int) throws(SocketError) -> Data {
        var received = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let n = read(fd, &buffer, buffer.count)
            if n < 0 {
                if errno == EINTR { continue }
                throw SocketError(description: "read: \(String(cString: strerror(errno)))")
            }
            if n == 0 { return received }
            received.append(contentsOf: buffer[0..<n])
            guard received.count <= limit else { throw SocketError(description: "response too large") }
        }
    }
}

public enum PeerCheck: Equatable {
    case trusted
    /// The process was found but does not satisfy the requirement.
    case untrusted(OSStatus)
    /// The process could not be looked up, for example because it exited.
    case unknown(OSStatus)
}

/// Checks the exact process an audit token names against a code requirement. A token stops
/// matching once its process exits or execs, so a reused pid never passes.
public func checkPeer(_ token: audit_token_t, requirement: String) -> PeerCheck {
    var token = token
    let tokenData = Data(bytes: &token, count: MemoryLayout<audit_token_t>.size)
    var code: SecCode?
    let attributes = [kSecGuestAttributeAudit: tokenData] as CFDictionary
    var status = SecCodeCopyGuestWithAttributes(nil, attributes, [], &code)
    guard status == errSecSuccess, let code else { return .unknown(status) }
    var compiled: SecRequirement?
    status = SecRequirementCreateWithString(requirement as CFString, [], &compiled)
    guard status == errSecSuccess, let compiled else { return .unknown(status) }
    status = SecCodeCheckValidity(code, [], compiled)
    return status == errSecSuccess ? .trusted : .untrusted(status)
}

/// berth-native writes a value only to valueFD: one end of a socketpair whose other end berth
/// run or berth-proxy holds. stdout carries nothing.
public let valueFD: Int32 = 3

public enum ValueChannel: Equatable, Sendable {
    /// The peer is the parent and satisfies the requirement for this kind of consumer.
    case parent(ConsumerKind)
    case refused(String)
}

/// Checks that fd is a connected Unix stream socket whose peer, by the kernel's audit token, is
/// the process parent and satisfies one of requirements, and says which. A pipe, FIFO, file or
/// terminal a wrapper put on fd instead is refused, and so is a socket handed through a wrapper
/// process, because its peer is then not the helper's parent.
public func checkValueChannel(_ fd: Int32, parent: pid_t, requirements: [(ConsumerKind, String)]) -> ValueChannel {
    var info = stat()
    guard fstat(fd, &info) == 0 else { return .refused("no value channel on fd \(fd)") }
    guard (info.st_mode & S_IFMT) == S_IFSOCK else { return .refused("the value channel is not a socket") }
    var type: Int32 = 0
    var typeSize = socklen_t(MemoryLayout<Int32>.size)
    guard getsockopt(fd, SOL_SOCKET, SO_TYPE, &type, &typeSize) == 0, type == SOCK_STREAM else {
        return .refused("the value channel is not a stream socket")
    }
    var address = sockaddr_storage()
    var addressSize = socklen_t(MemoryLayout<sockaddr_storage>.size)
    let named = withUnsafeMutablePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &addressSize) }
    }
    guard named == 0, Int32(address.ss_family) == AF_UNIX else {
        return .refused("the value channel is not a Unix socket")
    }
    var token = audit_token_t()
    var tokenSize = socklen_t(MemoryLayout<audit_token_t>.size)
    guard getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &tokenSize) == 0,
        tokenSize == socklen_t(MemoryLayout<audit_token_t>.size)
    else { return .refused("the value channel has no peer (\(String(cString: strerror(errno))))") }
    // For a socketpair the peer token names whoever holds the other end, which is how we know who receives.
    let peer = pid_t(bitPattern: token.val.5)
    guard peer == parent else { return .refused("the value channel's peer \(peer) is not the parent \(parent)") }
    var statuses: [String] = []
    for (kind, requirement) in requirements {
        switch checkPeer(token, requirement: requirement) {
        case .trusted: return .parent(kind)
        case .untrusted(let status): statuses.append("\(status)")
        case .unknown(let status): return .refused("cannot identify the value channel's peer (\(status))")
        }
    }
    return .refused(
        "the value channel's peer is not the signed berth or berth-proxy (\(statuses.joined(separator: ", ")))")
}
