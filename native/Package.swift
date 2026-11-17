// swift-tools-version: 6.3
import PackageDescription

let package = Package(
    name: "BerthNative",
    platforms: [.macOS(.v26)],
    products: [
        .executable(name: "berth-native", targets: ["BerthNative"])
    ],
    targets: [
        .target(name: "BerthNativeCore"),
        .executableTarget(name: "BerthNative", dependencies: ["BerthNativeCore"]),
        .testTarget(name: "BerthNativeCoreTests", dependencies: ["BerthNativeCore"]),
    ]
)
