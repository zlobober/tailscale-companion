// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "TailscaleCompanionMenu",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "TailscaleCompanionMenu", targets: ["CompanionMenu"])],
    targets: [
        .target(name: "CompanionCore"),
        .executableTarget(name: "CompanionMenu", dependencies: ["CompanionCore"],
                          linkerSettings: [.linkedFramework("AppKit")]),
        .testTarget(name: "CompanionCoreTests", dependencies: ["CompanionCore"])
    ]
)
