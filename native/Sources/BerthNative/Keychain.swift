import BerthNativeCore
import Foundation
import Security

/// A value buffer this process owns, so it can be wiped instead of waiting for a String or
/// Data copy to be freed.
final class SecretBuffer {
    private let storage: UnsafeMutableBufferPointer<UInt8>

    init?(copying text: String) {
        let bytes = text.utf8
        guard !bytes.isEmpty else { return nil }
        storage = .allocate(capacity: bytes.count)
        _ = storage.initialize(from: bytes)
    }

    /// A CFData view over the buffer with no copy, valid only until wipe.
    var cfData: CFData {
        CFDataCreateWithBytesNoCopy(nil, storage.baseAddress, storage.count, kCFAllocatorNull)
    }

    func wipe() {
        if let base = storage.baseAddress { memset_s(base, storage.count, 0, storage.count) }
    }

    deinit {
        wipe()
        storage.deallocate()
    }
}

enum Keychain {
    private static func query(_ name: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: keychainService(for: name),
            kSecAttrAccount as String: keychainAccount,
        ]
    }

    /// Asks for no data, so an existing item made by another app does not raise an access dialog.
    static func exists(_ name: String) -> Bool {
        var match = query(name)
        match[kSecMatchLimit as String] = kSecMatchLimitOne
        return SecItemCopyMatching(match as CFDictionary, nil) == errSecSuccess
    }

    /// Adds to the login keychain; the item's access list trusts this signed helper.
    static func add(_ name: String, value: SecretBuffer) -> OSStatus {
        var item = query(name)
        item[kSecAttrLabel as String] = keychainService(for: name)
        item[kSecValueData as String] = value.cfData
        return SecItemAdd(item as CFDictionary, nil)
    }

    /// Reads the item and hands its bytes to body; the bytes are wiped when body returns.
    static func withValue(_ name: String, _ body: (UnsafeRawBufferPointer) -> Bool) -> Result<Bool, OSStatusError> {
        var match = query(name)
        match[kSecReturnData as String] = true
        match[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(match as CFDictionary, &result)
        guard status == errSecSuccess, let result, CFGetTypeID(result) == CFDataGetTypeID() else {
            return .failure(OSStatusError(status: status))
        }
        // Not a downcast: the type ID was checked above, and CFData has no Swift subclass.
        let data = unsafeDowncast(result, to: CFData.self)
        let length = CFDataGetLength(data)
        guard let bytes = CFDataGetBytePtr(data) else {
            return .success(body(UnsafeRawBufferPointer(start: nil, count: 0)))
        }
        let ok = body(UnsafeRawBufferPointer(start: bytes, count: length))
        // The CFData is ours alone and about to be released; wiping it in place keeps the value
        // from lingering in freed memory.
        memset_s(UnsafeMutableRawPointer(mutating: bytes), length, 0, length)
        return .success(ok)
    }
}

struct OSStatusError: Error, CustomStringConvertible {
    var status: OSStatus

    var description: String {
        let message = SecCopyErrorMessageString(status, nil) as String? ?? "unknown"
        return "keychain status \(status) (\(message))"
    }
}
