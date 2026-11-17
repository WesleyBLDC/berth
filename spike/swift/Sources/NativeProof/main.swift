// A first sketch of berth-native: Touch ID, then a Keychain read whose bytes go only to stdout.
import Foundation
import LocalAuthentication
import Security

let service = "berth-spike-test"
let account = "berth"
let fakeValue = "not-a-real-secret-berth-spike"

func fail(_ message: String, _ code: Int32 = 1) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(code)
}

func baseQuery() -> [String: Any] {
    [kSecClass as String: kSecClassGenericPassword,
     kSecAttrService as String: service,
     kSecAttrAccount as String: account]
}

func add() {
    SecItemDelete(baseQuery() as CFDictionary)
    var query = baseQuery()
    query[kSecValueData as String] = Data(fakeValue.utf8)
    query[kSecAttrLabel as String] = "Berth spike test item (fake value)"
    let status = SecItemAdd(query as CFDictionary, nil)
    guard status == errSecSuccess else { fail("add failed: \(status)") }
    FileHandle.standardError.write(Data("added \(service)\n".utf8))
}

func authenticate(reason: String) {
    let context = LAContext()
    var error: NSError?
    guard context.canEvaluatePolicy(.deviceOwnerAuthentication, error: &error) else {
        fail("no device owner authentication: \(error?.localizedDescription ?? "unknown")", 2)
    }
    let done = DispatchSemaphore(value: 0)
    var approved = false
    var failure: Error?
    context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason) { ok, err in
        approved = ok
        failure = err
        done.signal()
    }
    done.wait()
    guard approved else { fail("declined: \(failure?.localizedDescription ?? "unknown")", 3) }
}

func read(reason: String) {
    authenticate(reason: reason)
    var query = baseQuery()
    query[kSecReturnData as String] = true
    query[kSecMatchLimit as String] = kSecMatchLimitOne
    var result: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &result)
    guard status == errSecSuccess, var data = result as? Data else { fail("read failed: \(status)") }
    FileHandle.standardOutput.write(data)
    data.resetBytes(in: 0..<data.count)
}

func delete() {
    let status = SecItemDelete(baseQuery() as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else { fail("delete failed: \(status)") }
    FileHandle.standardError.write(Data("deleted \(service)\n".utf8))
}

switch CommandLine.arguments.dropFirst().first {
case "add": add()
case "read": read(reason: CommandLine.arguments.dropFirst(2).first ?? "read the Berth spike test item")
case "delete": delete()
default: fail("usage: NativeProof add | read [reason] | delete")
}
