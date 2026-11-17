// Prints the CGWindowID of the first on-screen window whose title contains
// the argument, for `screencapture -l <id>` (captures that window only, even
// when other windows cover it). macOS only; needs Screen Recording permission
// for the app that runs it (window titles are hidden without it).
//
//   swiftc -O scripts/screenshots/windowid.swift -o <dir>/windowid
//   <dir>/windowid "ATN-SHOT"      # -> 12345, exit 1 if no window matches
//   <dir>/windowid --all "ATN-SHOT" # every matching window: id x y w h (popups too)
import CoreGraphics
import Foundation

var args = Array(CommandLine.arguments.dropFirst())
let all = args.first == "--all"
if all { args.removeFirst() }
guard let needle = args.first else {
    FileHandle.standardError.write("usage: windowid [--all] <title substring | owner pid>\n".data(using: .utf8)!)
    exit(2)
}
let pid = Int(needle)
let list = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
var found = false
for w in list {
    let name = w[kCGWindowName as String] as? String ?? ""
    let owner = w[kCGWindowOwnerPID as String] as? Int ?? -1
    guard (pid != nil && owner == pid) || (pid == nil && name.contains(needle)) else { continue }
    let id = w[kCGWindowNumber as String] as? Int ?? 0
    if all {
        let b = w[kCGWindowBounds as String] as? [String: Any] ?? [:]
        print("\(id) \(b["X"] ?? 0) \(b["Y"] ?? 0) \(b["Width"] ?? 0) \(b["Height"] ?? 0) \(w[kCGWindowLayer as String] ?? 0)")
        found = true
    } else if (w[kCGWindowLayer as String] as? Int ?? 0) == 0 {
        print(id)
        exit(0)
    }
}
exit(found ? 0 : 1)
