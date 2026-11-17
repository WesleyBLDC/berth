// swift-tools-version: 6.3
import PackageDescription

let package = Package(
    name: "BerthNative",
    platforms: [.macOS(.v26)],
    products: [
        .executable(name: "berth-native", targets: ["BerthNative"]),
        // The socket and peer check code Berth.app shares; it holds no Keychain code.
        .library(name: "BerthSocket", targets: ["BerthSocket"]),
    ],
    targets: [
        .target(name: "BerthSocket"),
        .target(name: "BerthNativeCore", dependencies: ["BerthSocket"]),
        .executableTarget(
            name: "BerthNative",
            dependencies: ["BerthNativeCore", "BerthSocket"],
            // Embedded in the binary so the signed helper never depends on a file beside it.
            resources: [.embedInCode("Resources/berth-icon.png")]
        ),
        .testTarget(name: "BerthNativeCoreTests", dependencies: ["BerthNativeCore", "BerthSocket"]),
    ]
)
