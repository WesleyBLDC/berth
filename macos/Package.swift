// swift-tools-version: 6.3
import PackageDescription

let package = Package(
    name: "BerthApp",
    platforms: [.macOS(.v26)],
    products: [
        .executable(name: "Berth", targets: ["BerthApp"]),
        .executable(name: "berth-app-probe", targets: ["BerthAppProbe"]),
    ],
    dependencies: [
        // Only the socket and peer check code; berth-native's Keychain code stays in its own target.
        .package(path: "../native")
    ],
    targets: [
        .target(
            name: "BerthAppCore",
            dependencies: [.product(name: "BerthSocket", package: "native")]
        ),
        .executableTarget(
            name: "BerthApp",
            dependencies: ["BerthAppCore"],
            // Embedded in the binary, copied from brand/menubar, so the bundle needs no resource bundle.
            resources: [.embedInCode("Resources/berthTemplate@2x.png")]
        ),
        // Run by the end to end tests against the real berthd; never bundled.
        .executableTarget(name: "BerthAppProbe", dependencies: ["BerthAppCore"], path: "Sources/berth-app-probe"),
        .testTarget(name: "BerthAppCoreTests", dependencies: ["BerthAppCore"], exclude: ["Fixtures"]),
        // Renders the app's views offscreen from the shared fixtures and compares them with Snapshots/.
        .testTarget(
            name: "BerthAppSnapshotTests", dependencies: ["BerthApp", "BerthAppCore"], exclude: ["Snapshots"]),
    ]
)
