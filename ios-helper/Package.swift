// swift-tools-version:5.9
//
// agemu-ios-helper: a long-lived helper process that drives iOS Simulators through
// CoreSimulator / SimulatorKit / AccessibilityPlatformTranslation private frameworks,
// loaded at runtime. It speaks newline-delimited JSON over stdio (see Sources/agemu-ios-helper/main.swift).
import PackageDescription

let package = Package(
    name: "agemu-ios-helper",
    platforms: [.macOS(.v13)],
    products: [
        .executable(name: "agemu-ios-helper", targets: ["agemu-ios-helper"]),
    ],
    targets: [
        // Objective-C glue for the private APIs. Objective-C so every call into CoreSimulator can be
        // wrapped in @try/@catch: those frameworks raise NSExceptions, which Swift cannot catch.
        .target(
            name: "AgemuSim",
            path: "Sources/AgemuSim",
            publicHeadersPath: "include",
            linkerSettings: [
                .linkedFramework("Foundation"),
                .linkedFramework("AppKit"),
                .linkedFramework("IOSurface"),
                .linkedFramework("CoreGraphics"),
                .linkedLibrary("z"),
            ]
        ),
        .executableTarget(
            name: "agemu-ios-helper",
            dependencies: ["AgemuSim"],
            path: "Sources/agemu-ios-helper",
            linkerSettings: [
                .linkedFramework("ImageIO"),
                .linkedFramework("CoreImage"),
            ]
        ),
    ]
)
