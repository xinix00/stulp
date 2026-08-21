// swift-tools-version: 6.2
import PackageDescription

let package = Package(
  name: "StulpCompanionCore",
  platforms: [
    .iOS(.v17),
    .macOS(.v14),
  ],
  products: [
    .library(name: "StulpCompanionCore", targets: ["StulpCompanionCore"])
  ],
  targets: [
    .target(
      name: "StulpCompanionCore",
      path: "Shared",
      linkerSettings: [.linkedFramework("Security")]
    ),
    .testTarget(
      name: "StulpCompanionCoreTests",
      dependencies: ["StulpCompanionCore"],
      path: "StulpTests"
    ),
  ]
)
