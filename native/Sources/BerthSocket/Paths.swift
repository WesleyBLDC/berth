import Foundation

/// Matches internal/paths in Go: $BERTH_HOME/berthd.sock, else under Application Support.
public func socketPath(environment: [String: String], fallbackHome: String) -> String {
    if let home = environment["BERTH_HOME"], !home.isEmpty {
        return (home as NSString).appendingPathComponent("berthd.sock")
    }
    let userHome = environment["HOME"].flatMap { $0.isEmpty ? nil : $0 } ?? fallbackHome
    return (userHome as NSString).appendingPathComponent("Library/Application Support/Berth/berthd.sock")
}
