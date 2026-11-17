import BerthSocket
import Darwin
import Foundation

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
