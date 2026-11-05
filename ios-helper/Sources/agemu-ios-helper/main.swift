// agemu-ios-helper: long-lived iOS Simulator control process for agemu.
//
// Protocol: newline-delimited JSON over stdio.
//   request : {"id": 1, "cmd": "screenshot", "udid": "...", ...params}
//   response: {"id": 1, "ok": true, "result": {...}, "ms": 12.3}
//             {"id": 1, "ok": false, "error": "message", "ms": 0.4}
// Requests are handled concurrently; HID input to one device is serialized.
// Coordinates sent to the helper are SCREEN PIXELS; accessibility frames come back in POINTS
// together with the scale (the Go backend converts them to pixels).
import AgemuSim
import Foundation

setvbuf(stdout, nil, _IOFBF, 1 << 16)

struct HelperError: Error, CustomStringConvertible {
    let description: String
    init(_ message: String) { description = message }
}

let outputLock = NSLock()

func send(_ object: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: object, options: []) else {
        return
    }
    outputLock.lock()
    defer { outputLock.unlock() }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write("\n".data(using: .utf8)!)
}

func loadFrameworks() throws {
    var err: NSError?
    if !AGLoadFrameworks(&err) { throw err ?? HelperError("loading private frameworks failed") }
}

func log(_ message: String) {
    FileHandle.standardError.write("agemu-ios-helper: \(message)\n".data(using: .utf8)!)
}

// Per-device serial queues for input, so taps and keystrokes from concurrent requests never interleave.
let inputQueuesLock = NSLock()
var inputQueues: [String: DispatchQueue] = [:]
func inputQueue(_ udid: String) -> DispatchQueue {
    inputQueuesLock.lock()
    defer { inputQueuesLock.unlock() }
    if let q = inputQueues[udid] { return q }
    let q = DispatchQueue(label: "agemu.input.\(udid)")
    inputQueues[udid] = q
    return q
}

extension Dictionary where Key == String, Value == Any {
    func string(_ key: String) throws -> String {
        guard let v = self[key] as? String, !v.isEmpty else { throw HelperError("missing \(key)") }
        return v
    }
    func optString(_ key: String) -> String? { self[key] as? String }
    func double(_ key: String) throws -> Double {
        if let v = self[key] as? Double { return v }
        if let v = self[key] as? Int { return Double(v) }
        if let v = self[key] as? NSNumber { return v.doubleValue }
        throw HelperError("missing \(key)")
    }
    func optDouble(_ key: String, _ def: Double) -> Double { (try? double(key)) ?? def }
    func optBool(_ key: String) -> Bool { (self[key] as? Bool) ?? false }
}

func device(_ req: [String: Any]) throws -> AGDevice {
    try AGSimulators.device(withUDID: try req.string("udid"))
}

func describe(_ d: AGDevice) -> [String: Any] {
    let px = d.screenPixels
    return [
        "udid": d.udid,
        "name": d.name,
        "state": d.stateString,
        "stateCode": d.state,
        "deviceType": d.deviceTypeIdentifier,
        "deviceTypeName": d.deviceTypeName,
        "runtime": d.runtimeIdentifier,
        "runtimeVersion": d.runtimeVersion,
        "width": Int(px.width),
        "height": Int(px.height),
        "scale": d.screenScale,
    ]
}

func sleepMs(_ ms: Double) {
    if ms > 0 { usleep(useconds_t(ms * 1000)) }
}

// MARK: - Input

func ratio(_ d: AGDevice, _ x: Double, _ y: Double) throws -> (Double, Double) {
    let px = d.screenPixels
    guard px.width > 0, px.height > 0 else { throw HelperError("unknown screen size for \(d.udid)") }
    return (x / Double(px.width), y / Double(px.height))
}

func tap(_ d: AGDevice, x: Double, y: Double, holdMs: Double) throws {
    let (rx, ry) = try ratio(d, x, y)
    try d.touchRatioX(rx, y: ry, down: true, edge: .none)
    sleepMs(holdMs)
    try d.touchRatioX(rx, y: ry, down: false, edge: .none)
    try d.hidWait(forAcknowledgements: 5)
}

func swipe(_ d: AGDevice, x1: Double, y1: Double, x2: Double, y2: Double, durationMs: Double, edge: AGEdge) throws {
    let (ax, ay) = try ratio(d, x1, y1)
    let (bx, by) = try ratio(d, x2, y2)
    // One sample per ~16 ms (a 60 Hz digitizer), at least 4.
    let steps = max(4, Int(durationMs / 16))
    let stepDelay = durationMs / Double(steps)
    try d.touchRatioX(ax, y: ay, down: true, edge: edge)
    for i in 1...steps {
        sleepMs(stepDelay)
        let t = Double(i) / Double(steps)
        try d.touchRatioX(ax + (bx - ax) * t, y: ay + (by - ay) * t, down: true, edge: edge)
    }
    // A final stationary sample stops the content from flinging (as idb does).
    sleepMs(stepDelay)
    try d.touchRatioX(bx, y: by, down: true, edge: edge)
    try d.touchRatioX(bx, y: by, down: false, edge: edge)
    try d.hidWait(forAcknowledgements: 5)
}

// HID keyboard usages (page 7) for US-layout characters: (usage, shift).
let keyMap: [Character: (UInt32, Bool)] = {
    var m: [Character: (UInt32, Bool)] = [:]
    for (i, c) in "abcdefghijklmnopqrstuvwxyz".enumerated() {
        m[c] = (UInt32(4 + i), false)
        m[Character(c.uppercased())] = (UInt32(4 + i), true)
    }
    for (i, c) in "1234567890".enumerated() { m[c] = (UInt32(30 + i), false) }
    for (i, c) in "!@#$%^&*()".enumerated() { m[c] = (UInt32(30 + i), true) }
    let plain: [(Character, UInt32)] = [
        ("\n", 40), ("\t", 43), (" ", 44), ("-", 45), ("=", 46), ("[", 47), ("]", 48), ("\\", 49),
        (";", 51), ("'", 52), ("`", 53), (",", 54), (".", 55), ("/", 56),
    ]
    for (c, u) in plain { m[c] = (u, false) }
    let shifted: [(Character, UInt32)] = [
        ("_", 45), ("+", 46), ("{", 47), ("}", 48), ("|", 49), (":", 51), ("\"", 52), ("~", 53),
        ("<", 54), (">", 55), ("?", 56),
    ]
    for (c, u) in shifted { m[c] = (u, true) }
    return m
}()

let leftShift: UInt32 = 225
let leftGUI: UInt32 = 227

func typeText(_ d: AGDevice, _ text: String) throws {
    var unsupported = Set<Character>()
    for c in text where keyMap[c] == nil { unsupported.insert(c) }
    if !unsupported.isEmpty {
        throw HelperError("cannot type characters \(unsupported.map { String($0) }.sorted()) with the US hardware keyboard; use paste")
    }
    for c in text {
        let (usage, shift) = keyMap[c]!
        if shift { try d.key(leftShift, down: true) }
        try d.key(usage, down: true)
        try d.key(usage, down: false)
        if shift { try d.key(leftShift, down: false) }
    }
    try d.hidWait(forAcknowledgements: 10)
}

func pressKey(_ d: AGDevice, _ name: String) throws {
    let usages: [String: UInt32] = [
        "enter": 40, "return": 40, "escape": 41, "esc": 41, "delete": 42, "backspace": 42, "tab": 43,
        "space": 44, "right": 79, "left": 80, "down": 81, "up": 82,
    ]
    switch name {
    case "home":
        try d.button(0, down: true)
        try d.button(0, down: false)
    case "lock", "power":
        try d.button(1, down: true)
        try d.button(1, down: false)
    case "volumeup":
        try d.consumerUsage(0xE9, down: true)
        try d.consumerUsage(0xE9, down: false)
    case "volumedown":
        try d.consumerUsage(0xEA, down: true)
        try d.consumerUsage(0xEA, down: false)
    case "paste":
        try d.key(leftGUI, down: true)
        try d.key(25, down: true)  // v
        try d.key(25, down: false)
        try d.key(leftGUI, down: false)
    case "back":
        // iOS has no back key: an edge swipe from the left edge triggers the interactive pop
        // gesture of navigation controllers, like a user's thumb.
        let px = d.screenPixels
        try swipe(d, x1: 0, y1: Double(px.height) * 0.5, x2: Double(px.width) * 0.75, y2: Double(px.height) * 0.5,
                  durationMs: 200, edge: .left)
        return
    default:
        guard let u = usages[name] else {
            throw HelperError("unknown key \(name) (home, lock, back, enter, delete, tab, escape, space, up, down, left, right, volumeup, volumedown, paste)")
        }
        try d.key(u, down: true)
        try d.key(u, down: false)
    }
    try d.hidWait(forAcknowledgements: 5)
}

// MARK: - Running apps

// Parses `launchctl list` from inside the simulator: UIKitApplication:<bundle>[...] entries.
func runningApps(_ d: AGDevice) throws -> [[String: Any]] {
    let out = try d.spawnCapture("bin/launchctl", arguments: ["list"], timeout: 10)
    var apps: [[String: Any]] = []
    for line in out.split(separator: "\n") {
        let cols = line.split(separator: "\t", omittingEmptySubsequences: false)
        guard cols.count >= 3, let pid = Int(cols[0]) else { continue }
        let label = String(cols[2])
        if label == "com.apple.SpringBoard" {
            apps.append(["bundleId": "com.apple.springboard", "pid": pid])
            continue
        }
        guard label.hasPrefix("UIKitApplication:") else { continue }
        var bundle = String(label.dropFirst("UIKitApplication:".count))
        if let bracket = bundle.firstIndex(of: "[") { bundle = String(bundle[..<bracket]) }
        apps.append(["bundleId": bundle, "pid": pid])
    }
    return apps
}

// MARK: - App accessibility switch

// Apps load their accessibility runtime only while ApplicationAccessibilityEnabled is on in the
// simulator. XCUITest-based tools (Maestro, `xcodebuild test`) switch it on for their session and
// back off afterwards, which leaves apps launched later unreadable (a zero-sized root, no
// children). agemu switches it on, as any UI automation does, and posts the change notifications
// so running apps load their accessibility runtime without a relaunch.
let axSwitchLock = NSLock()
var axSwitchedAt: [String: Date] = [:]

@discardableResult
func ensureAppAccessibility(_ d: AGDevice, force: Bool) throws -> Bool {
    axSwitchLock.lock()
    defer { axSwitchLock.unlock() }
    if !force, let t = axSwitchedAt[d.udid], Date().timeIntervalSince(t) < 3 { return false }
    _ = try d.spawnCapture(
        "usr/bin/defaults",
        arguments: ["write", "com.apple.Accessibility", "ApplicationAccessibilityEnabled", "-bool", "true"],
        timeout: 10)
    try d.postDarwinNotification("com.apple.accessibility.cache.app.ax")
    try d.postDarwinNotification("com.apple.accessibility.cache.ax")
    axSwitchedAt[d.udid] = Date()
    return true
}

func zeroRoot(_ tree: [String: Any]) -> Bool {
    guard let root = tree["root"] as? [String: Any], let frame = root["frame"] as? [Double], frame.count == 4 else {
        return true
    }
    return frame[2] <= 0 || frame[3] <= 0
}

// Reads the frontmost app's tree. A live app with a zero-sized root has no accessibility runtime
// loaded: switch app accessibility on and retry for up to 4 s.
func readTree(_ d: AGDevice, maxDepth: Int) throws -> [String: Any] {
    var tree = try d.accessibilityTree(withMaxDepth: maxDepth) as! [String: Any]
    if zeroRoot(tree), (tree["pid"] as? Int ?? 0) > 0, try ensureAppAccessibility(d, force: false) {
        let deadline = Date().addingTimeInterval(4)
        while zeroRoot(tree), Date() < deadline {
            sleepMs(100)
            tree = try d.accessibilityTree(withMaxDepth: maxDepth) as! [String: Any]
        }
        tree["accessibilitySwitchedOn"] = true
    }
    return tree
}

// MARK: - User apps

// User-installed apps live in the device's data directory on the host
// (Containers/Bundle/Application/<uuid>/<name>.app). Reading their Info.plists takes
// milliseconds; -[SimDevice installedAppsWithError:] takes ~1-2 s.
func userApps(_ d: AGDevice) -> [String] {
    let root = URL(fileURLWithPath: d.dataPath).appendingPathComponent("Containers/Bundle/Application")
    let fm = FileManager.default
    var out: [String] = []
    for container in (try? fm.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? [] {
        for item in (try? fm.contentsOfDirectory(at: container, includingPropertiesForKeys: nil)) ?? []
        where item.pathExtension == "app" {
            guard let data = try? Data(contentsOf: item.appendingPathComponent("Info.plist")),
                  let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
                  let bundle = plist["CFBundleIdentifier"] as? String
            else { continue }
            out.append(bundle)
        }
    }
    return out.sorted()
}

// MARK: - Readiness

// Agent-ready: booted AND SpringBoard answers accessibility AND the framebuffer is readable.
func waitReady(_ d: AGDevice, timeoutMs: Double) throws -> [String: Any] {
    let start = Date()
    let deadline = start.addingTimeInterval(timeoutMs / 1000)
    var bootedAt: Double = -1
    var lastError = "not booted"
    // A device that boots now gets app accessibility switched on once it is up; an already
    // booted one is only fixed lazily (see readTree), to keep `boot` instant.
    var needAXSwitch = d.state != 3
    while Date() < deadline {
        if d.state == 3 {
            if bootedAt < 0 { bootedAt = Date().timeIntervalSince(start) * 1000 }
            do {
                if needAXSwitch {
                    try ensureAppAccessibility(d, force: true)
                    needAXSwitch = false
                }
                try d.prepareHID()
                let surface = try d.framebuffer()
                let front = try d.accessibilityFrontmost()
                let frame = front["frame"] as? [Double] ?? []
                let pid = front["pid"] as? Int ?? 0
                if pid > 0, frame.count == 4, frame[2] > 0, frame[3] > 0, surface.width > 0 {
                    return [
                        "bootedMs": bootedAt,
                        "readyMs": Date().timeIntervalSince(start) * 1000,
                        "frontmostPid": pid,
                        "frontmost": front["label"] ?? "",
                    ]
                }
                lastError = "frontmost application has no frame yet"
            } catch {
                lastError = "\(error.localizedDescription)"
            }
        } else {
            lastError = "state \(d.stateString)"
        }
        sleepMs(50)
    }
    throw HelperError("device \(d.udid) not agent-ready after \(Int(timeoutMs)) ms: \(lastError)")
}

// MARK: - Dispatch

func handle(_ req: [String: Any]) throws -> Any {
    let cmd = try req.string("cmd")
    switch cmd {
    case "ping":
        return ["pid": getpid()]
    case "hello":
        try loadFrameworks()
        return ["pid": getpid(), "versions": AGFrameworkVersions()]
    case "list":
        return try AGSimulators.devices().map(describe)
    case "describe":
        return describe(try device(req))
    case "boot":
        let d = try device(req)
        if d.state != 3 && d.state != 2 { try d.boot() }
        return describe(d)
    case "shutdown":
        let d = try device(req)
        if d.state != 1 { try d.shutdown() }
        return describe(d)
    case "ready":
        return try waitReady(try device(req), timeoutMs: req.optDouble("timeoutMs", 120_000))
    case "screenshot":
        let d = try device(req)
        return try Screenshot.capture(
            d, path: try req.string("path"), format: req.optString("format") ?? "png",
            scale: req.optDouble("scale", 1), quality: req.optDouble("quality", 0.85))
    case "tap":
        let d = try device(req)
        let x = try req.double("x"), y = try req.double("y")
        let hold = req.optDouble("holdMs", 0)
        try inputQueue(d.udid).sync { try tap(d, x: x, y: y, holdMs: hold) }
        return ["ok": true]
    case "swipe":
        let d = try device(req)
        let edge = AGEdge(rawValue: UInt32(req.optDouble("edge", 0))) ?? AGEdge.none
        let args = (try req.double("x1"), try req.double("y1"), try req.double("x2"), try req.double("y2"))
        let ms = req.optDouble("durationMs", 300)
        try inputQueue(d.udid).sync {
            try swipe(d, x1: args.0, y1: args.1, x2: args.2, y2: args.3, durationMs: ms, edge: edge)
        }
        return ["ok": true]
    case "type":
        let d = try device(req)
        let text = req.optString("text") ?? ""
        try inputQueue(d.udid).sync { try typeText(d, text) }
        return ["ok": true]
    case "key":
        let d = try device(req)
        let key = try req.string("key").lowercased()
        try inputQueue(d.udid).sync { try pressKey(d, key) }
        return ["ok": true]
    case "ui":
        let d = try device(req)
        let depth = Int(req.optDouble("maxDepth", -1))
        var tree = try readTree(d, maxDepth: depth)
        tree["scale"] = d.screenScale
        tree["width"] = Int(d.screenPixels.width)
        tree["height"] = Int(d.screenPixels.height)
        return tree
    case "hit":
        let d = try device(req)
        return try d.accessibility(atPointX: try req.double("x"), y: try req.double("y"), maxDepth: Int(req.optDouble("maxDepth", 3)))
    case "ensure-ax":
        return ["switched": try ensureAppAccessibility(try device(req), force: true)]
    case "frontmost":
        return try device(req).accessibilityFrontmost()
    case "launch":
        let d = try device(req)
        let bundle = try req.string("bundleId")
        if req.optBool("terminateExisting") { try? d.terminate(bundle) }
        let args = req["args"] as? [String] ?? []
        let env = req["env"] as? [String: String] ?? [:]
        var pid: Int32 = -1
        try ObjCError.call { err in pid = d.launch(bundle, arguments: args, environment: env, error: err) }
        return ["pid": pid]
    case "terminate":
        try device(req).terminate(try req.string("bundleId"))
        return ["ok": true]
    case "install":
        try device(req).install(try req.string("path"))
        return ["ok": true]
    case "uninstall":
        try device(req).uninstall(try req.string("bundleId"))
        return ["ok": true]
    case "apps":
        let apps = try device(req).installedApps()
        var out: [String: Any] = [:]
        for (bundle, info) in apps {
            out[bundle] = [
                "type": info["ApplicationType"] ?? "",
                "name": info["CFBundleDisplayName"] ?? info["CFBundleName"] ?? "",
                "path": info["Path"] ?? "",
            ]
        }
        return out
    case "userapps":
        return userApps(try device(req))
    case "running":
        return try runningApps(try device(req))
    case "spawn":
        let d = try device(req)
        let out = try d.spawnCapture(try req.string("path"), arguments: req["args"] as? [String] ?? [],
                                     timeout: req.optDouble("timeoutMs", 10000) / 1000)
        return ["stdout": out]
    case "notify":
        try device(req).postDarwinNotification(try req.string("name"))
        return ["ok": true]
    default:
        throw HelperError("unknown cmd \(cmd)")
    }
}

/// Bridges Objective-C methods whose failure is signalled by a sentinel return value.
enum ObjCError {
    static func call(_ body: (NSErrorPointer) -> Void) throws {
        var err: NSError?
        body(&err)
        if let err { throw err }
    }
}

func process(_ line: Data) {
    let start = Date()
    var id: Any = NSNull()
    do {
        guard let req = try JSONSerialization.jsonObject(with: line) as? [String: Any] else {
            throw HelperError("request must be a JSON object")
        }
        id = req["id"] ?? NSNull()
        let result = try handle(req)
        send(["id": id, "ok": true, "result": result, "ms": Date().timeIntervalSince(start) * 1000])
    } catch {
        let message = (error as? HelperError)?.description ?? error.localizedDescription
        send(["id": id, "ok": false, "error": message, "ms": Date().timeIntervalSince(start) * 1000])
    }
}

do {
    try loadFrameworks()
} catch {
    log("loading private frameworks failed: \(error.localizedDescription)")
}

let reader = Thread {
    let stdin = FileHandle.standardInput
    var buffer = Data()
    while true {
        let chunk = stdin.availableData
        if chunk.isEmpty { exit(0) }  // stdin closed: the parent is gone
        buffer.append(chunk)
        while let nl = buffer.firstIndex(of: 0x0A) {
            let line = buffer.subdata(in: buffer.startIndex..<nl)
            buffer.removeSubrange(buffer.startIndex...nl)
            if line.isEmpty { continue }
            DispatchQueue.global(qos: .userInitiated).async { process(line) }
        }
    }
}
reader.start()
RunLoop.main.run()
