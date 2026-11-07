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
        "dataPath": d.dataPath,
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

/// Returns the display's frame mark taken right before the last key, so the caller waits for the
/// frame that shows the last character and not for one an earlier key produced.
func typeText(_ d: AGDevice, _ text: String) throws -> UInt {
    var unsupported = Set<Character>()
    for c in text where keyMap[c] == nil { unsupported.insert(c) }
    if !unsupported.isEmpty {
        throw HelperError("cannot type characters \(unsupported.map { String($0) }.sorted()) with the US hardware keyboard; use paste")
    }
    // Keys are paced (each acknowledged, then AGEMU_IOS_KEY_GAP_MS, default 10). That is not
    // enough for every React Native controlled input: in an Expo development build key events
    // lost or duplicated characters at any pace tried (a burst kept 11 of 26 e-mail characters;
    // a 10 ms gap lost 1 of 6 one-time-code digits; waiting for each key's frame lost "ex" in one
    // try and doubled ".pl" in another). That is why `type` pastes by default; key events are
    // the explicit `--keys` path.
    var mark = d.inputFrameMark()
    let last = text.count - 1
    for (i, c) in text.enumerated() {
        if i == last && i > 0 { mark = d.inputFrameMark() }
        let (usage, shift) = keyMap[c]!
        if shift { try d.key(leftShift, down: true) }
        try d.key(usage, down: true)
        try d.key(usage, down: false)
        if shift { try d.key(leftShift, down: false) }
        try d.hidWait(forAcknowledgements: 10)
        if keyGapMs > 0 { usleep(keyGapMs * 1000) }
    }
    return mark
}

let keyGapMs: UInt32 = UInt32(ProcessInfo.processInfo.environment["AGEMU_IOS_KEY_GAP_MS"] ?? "") ?? 10

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
        try swipe(d, x1: 0, y1: Double(px.height) * 0.5, x2: Double(px.width) * 0.8, y2: Double(px.height) * 0.5,
                  durationMs: 350, edge: .left)
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

/// AGEMU_IOS_TAP_SETTLE=0 makes taps skip the wait for a still screen.
let tapSettle = ProcessInfo.processInfo.environment["AGEMU_IOS_TAP_SETTLE"] != "0"

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

// An iPad tree whose app no hit test confirmed as the screen's owner, and that has no elements.
func unconfirmedEmpty(_ tree: [String: Any]) -> Bool {
    guard tree["unconfirmed"] as? Bool == true else { return false }
    let root = tree["root"] as? [String: Any]
    return (root?["children"] as? [Any])?.isEmpty ?? true
}

// Reads the frontmost app's tree within timeout seconds. A live app with a zero-sized root has
// no accessibility runtime loaded: switch app accessibility on and retry for up to 4 s.
func readTree(_ d: AGDevice, maxDepth: Int, timeout: Double) throws -> [String: Any] {
    let deadline = Date().addingTimeInterval(timeout)
    // Right after a boot, accessibility may still be warming up: wait for it (within budget).
    AXWarmup.waitIfRunning(d, until: deadline)
    var tree: [String: Any]
    do {
        tree = try d.accessibilityTree(withMaxDepth: maxDepth, timeout: max(0.1, deadline.timeIntervalSinceNow)) as! [String: Any]
        bridgeDoctor.noteResult(d, error: nil)
        noteAccessibilityAnswered(d)
    } catch {
        bridgeDoctor.noteResult(d, error: error)
        throw error
    }
    // iPad: for about a second after an app is launched, accessibility reports the dock's
    // DockFolderViewService as the frontmost app and hit tests find nothing on the screen yet
    // (AgemuSim's screenOwnerForFrontmost). Wait for the screen's owner (at most 3 s, within the
    // budget) instead of returning that empty tree.
    if unconfirmedEmpty(tree) {
        let until = min(deadline, Date().addingTimeInterval(3))
        while unconfirmedEmpty(tree), Date() < until {
            sleepMs(100)
            let left = deadline.timeIntervalSinceNow
            if left <= 0 { break }
            tree = try d.accessibilityTree(withMaxDepth: maxDepth, timeout: left) as! [String: Any]
        }
    }
    if zeroRoot(tree), (tree["pid"] as? Int ?? 0) > 0, try ensureAppAccessibility(d, force: false) {
        let until = min(deadline, Date().addingTimeInterval(4))
        while zeroRoot(tree), Date() < until {
            sleepMs(100)
            let left = deadline.timeIntervalSinceNow
            if left <= 0 { break }
            tree = try d.accessibilityTree(withMaxDepth: maxDepth, timeout: left) as! [String: Any]
        }
        tree["accessibilitySwitchedOn"] = true
    }
    return tree
}

// MARK: - Accessibility bridge repair

// CoreSimulatorBridge (inside the simulator) answers accessibility requests. It can lose track
// of SpringBoard, for example when SpringBoard is killed and relaunched while the bridge runs,
// and then answers "no frontmost application" to every client until it is restarted. Restarting
// it (launchctl kickstart -k inside that simulator) takes 1-3 s and is harmless.
func restartBridge(_ d: AGDevice, reason: String) {
    log("\(d.udid): \(reason); restarting CoreSimulatorBridge")
    let t = Date()
    do {
        _ = try d.spawnCapture("bin/launchctl", arguments: ["kickstart", "-k", "system/com.apple.CoreSimulator.bridge"], timeout: 15)
        log("\(d.udid): CoreSimulatorBridge restarted in \(Int(Date().timeIntervalSince(t) * 1000)) ms")
    } catch {
        log("\(d.udid): restarting CoreSimulatorBridge failed: \(error.localizedDescription)")
    }
}

func isNoFrontmost(_ error: Error?) -> Bool {
    guard let error else { return false }
    let m = (error as? HelperError)?.description ?? error.localizedDescription
    return m.contains("no frontmost application")
}

// Outside boot (see waitReady), failures are noted here and checked in the background, so a
// failing read returns its error at once instead of waiting for launchctl. When accessibility
// has said "no frontmost application" for 2 s while boot has finished and SpringBoard has run for
// 2 s, the bridge is restarted: at most twice per SpringBoard instance, and not within 15 s of
// the previous restart.
final class BridgeDoctor {
    private let lock = NSLock()
    private var failingSince: [String: Date] = [:]
    private var checking: Set<String> = []
    private var lastRestart: [String: Date] = [:]
    private var springboard: [String: (pid: Int, seen: Date)] = [:]
    private var restartsForSpringBoard: [String: (pid: Int, count: Int)] = [:]

    func noteResult(_ d: AGDevice, error: Error?) {
        let udid = d.udid
        lock.lock()
        guard isNoFrontmost(error) else {
            if error == nil { failingSince[udid] = nil }
            lock.unlock()
            return
        }
        let since = failingSince[udid] ?? Date()
        failingSince[udid] = since
        let start = Date().timeIntervalSince(since) >= 2 && !checking.contains(udid)
        if start { checking.insert(udid) }
        lock.unlock()
        if start {
            DispatchQueue.global(qos: .userInitiated).async { self.check(d) }
        }
    }

    // Readiness restarts the bridge itself; this keeps the doctor from restarting it again
    // right after.
    func noteRestart(_ d: AGDevice) {
        lock.lock()
        lastRestart[d.udid] = Date()
        failingSince[d.udid] = nil
        lock.unlock()
    }

    private func check(_ d: AGDevice) {
        let udid = d.udid
        defer {
            lock.lock()
            checking.remove(udid)
            lock.unlock()
        }
        if let bs = d.bootStatusInfo, !((bs["terminal"] as? Bool) ?? true) { return }  // still booting
        let apps = (try? runningApps(d)) ?? []
        guard let sbPid = apps.first(where: { ($0["bundleId"] as? String) == "com.apple.springboard" })?["pid"] as? Int else {
            return  // SpringBoard is not running (yet)
        }
        lock.lock()
        if springboard[udid]?.pid != sbPid { springboard[udid] = (sbPid, Date()) }
        let upFor = Date().timeIntervalSince(springboard[udid]!.seen)
        let failingFor = failingSince[udid].map { Date().timeIntervalSince($0) } ?? 0
        let recent = lastRestart[udid].map { Date().timeIntervalSince($0) < 15 } ?? false
        var r = restartsForSpringBoard[udid] ?? (sbPid, 0)
        if r.pid != sbPid { r = (sbPid, 0) }
        let go = failingFor >= 2 && upFor >= 2 && !recent && r.count < 2
        if go {
            r.count += 1
            restartsForSpringBoard[udid] = r
            lastRestart[udid] = Date()
            failingSince[udid] = nil
        }
        lock.unlock()
        if go {
            restartBridge(d, reason: "no frontmost app from accessibility for \(Int(failingFor)) s while SpringBoard (pid \(sbPid)) runs")
        }
    }
}

let bridgeDoctor = BridgeDoctor()

// MARK: - User apps

// User-installed apps live in the device's data directory on the host
// (Containers/Bundle/Application/<uuid>/<name>.app). Reading their Info.plists takes
// milliseconds; -[SimDevice installedAppsWithError:] takes ~1-2 s.
func userApps(_ d: AGDevice) -> [String] {
    userAppInfos(d).compactMap { $0["id"] as? String }
}

// MARK: - Readiness

/// Thread-safe record of when each readiness milestone happened (ms since the request).
final class ReadyTrace {
    let start = Date()
    private let lock = NSLock()
    private var events: [[String: Any]] = []
    private var marks: [String: Double] = [:]

    func ms() -> Double { (Date().timeIntervalSince(start) * 1000).rounded() }

    func mark(_ name: String, _ detail: String = "") {
        lock.lock()
        defer { lock.unlock() }
        let t = ms()
        if marks[name] == nil { marks[name] = t }
        var e: [String: Any] = ["ms": t, "event": name]
        if !detail.isEmpty { e["detail"] = detail }
        events.append(e)
    }

    func has(_ name: String) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return marks[name] != nil
    }

    func at(_ name: String) -> Double? {
        lock.lock()
        defer { lock.unlock() }
        return marks[name]
    }

    var timeline: [[String: Any]] {
        lock.lock()
        defer { lock.unlock() }
        return events
    }

    var summary: String {
        timeline.map { e in
            "\(Int(e["ms"] as? Double ?? 0)) \(e["event"] ?? "")" + ((e["detail"] as? String).map { " (\($0))" } ?? "")
        }.joined(separator: ", ")
    }
}

func bootStatusName(_ status: Int64) -> String {
    switch status {
    case 0: return "booting"
    case 1: return "waiting on BackBoard"
    case 2: return "waiting on data migration"
    case 4: return "waiting on SpringBoard"
    case 0xFFFF_FFFF, -1: return "finished"
    default: return "status \(status)"
    }
}

/// Boots that have already been verified agent-ready (udid -> -[SimDevice lastBootedAt]); a
/// ready check on them only asks for the frontmost app's pid (one accessibility request), and
/// nothing at all when accessibility answered within the last 5 s (axAnsweredAt).
let verifiedLock = NSLock()
var verifiedBoots: [String: Date] = [:]
var axAnsweredAt: [String: (boot: Date?, at: Date)] = [:]

func noteAccessibilityAnswered(_ d: AGDevice) {
    verifiedLock.lock()
    axAnsweredAt[d.udid] = (d.lastBootedAt, Date())
    verifiedLock.unlock()
}

func accessibilityAnsweredRecently(_ d: AGDevice, within: TimeInterval) -> Bool {
    verifiedLock.lock()
    defer { verifiedLock.unlock() }
    guard let a = axAnsweredAt[d.udid] else { return false }
    return a.boot == d.lastBootedAt && Date().timeIntervalSince(a.at) < within
}

/// How long after boot finished accessibility may keep saying "no frontmost application"
/// before the bridge is restarted. Measured (docs/IOS.md): the bridge retries finding
/// SpringBoard every 5 s by itself and answered 1.5-2.8 s after boot finished; restarting it at
/// once made that 4-7 s. So restart only once its own retry has had its chance.
/// AGEMU_IOS_BRIDGE_RESTART_MS overrides (for experiments).
let firstBridgeRestartAfter: Double = {
    if let v = ProcessInfo.processInfo.environment["AGEMU_IOS_BRIDGE_RESTART_MS"], let ms = Double(v) { return ms / 1000 }
    return 6
}()

/// Waits until the simulator's accessibility service answers with a frontmost app, restarting
/// CoreSimulatorBridge if it keeps saying "no frontmost application" (first after
/// firstBridgeRestartAfter, then every 10 s, at most 3 times). One per device and boot: it
/// stops when the device shuts down or boots again. UI reads wait for a running warm-up
/// instead of failing.
final class AXWarmup {
    private static let lock = NSLock()
    private static var running: [String: AXWarmup] = [:]

    let udid: String
    let boot: Date?
    private let done = DispatchSemaphore(value: 0)
    private let stateLock = NSLock()
    private var finished = false
    private var cancelled = false
    private(set) var pid = 0
    private(set) var label = ""
    private(set) var restarts = 0
    private(set) var lastError = ""

    private init(udid: String, boot: Date?) {
        self.udid = udid
        self.boot = boot
    }

    private var isCancelled: Bool {
        stateLock.lock()
        defer { stateLock.unlock() }
        return cancelled
    }

    /// Starts a warm-up for d's current boot unless one is running; returns the running one.
    /// A warm-up left over from an earlier boot is stopped.
    static func start(_ d: AGDevice, trace: ReadyTrace, full: Bool, deadline: Date) -> AXWarmup {
        let boot = d.lastBootedAt
        lock.lock()
        if let w = running[d.udid] {
            if w.boot == boot {
                lock.unlock()
                return w
            }
            w.stateLock.lock()
            w.cancelled = true
            w.stateLock.unlock()
        }
        let w = AXWarmup(udid: d.udid, boot: boot)
        running[d.udid] = w
        lock.unlock()
        Thread.detachNewThread { w.run(d, trace: trace, full: full, deadline: deadline) }
        return w
    }

    /// Waits (until the given time) for a warm-up running on d, if any.
    static func waitIfRunning(_ d: AGDevice, until: Date) {
        lock.lock()
        let w = running[d.udid]
        lock.unlock()
        _ = w?.wait(until: until)
    }

    var isDone: Bool {
        stateLock.lock()
        defer { stateLock.unlock() }
        return finished
    }

    /// true once accessibility answered.
    func wait(until: Date) -> Bool {
        if isDone { return pid > 0 }
        let left = until.timeIntervalSinceNow
        if left <= 0 { return false }
        if done.wait(timeout: .now() + left) == .success {
            done.signal()  // let other waiters through
        }
        return isDone && pid > 0
    }

    private func run(_ d: AGDevice, trace: ReadyTrace, full: Bool, deadline: Date) {
        let started = Date()
        var lastRestartAt: Date?
        while Date() < deadline, !isCancelled, d.state == 3, d.lastBootedAt == boot {
            do {
                let front = try d.accessibilityFrontmost(withTimeout: max(0.05, min(1.5, deadline.timeIntervalSinceNow)), full: full)
                let p = front["pid"] as? Int ?? 0
                let frame = front["frame"] as? [Double] ?? []
                if p > 0, !full || (frame.count == 4 && frame[2] > 0 && frame[3] > 0) {
                    pid = p
                    label = front["label"] as? String ?? ""
                    trace.mark("accessibility", "frontmost pid \(p)")
                    bridgeDoctor.noteResult(d, error: nil)
                    noteAccessibilityAnswered(d)
                    break
                }
                lastError = "frontmost application has no frame yet"
            } catch {
                let msg = (error as? HelperError)?.description ?? error.localizedDescription
                if msg != lastError { trace.mark("accessibilityFails", String(msg.prefix(120))) }
                lastError = msg
            }
            if restarts < 3, !isCancelled, lastError.contains("no frontmost application") {
                let since = lastRestartAt ?? started
                let wait: Double = restarts == 0 ? firstBridgeRestartAfter : 10
                if Date().timeIntervalSince(since) >= wait {
                    restarts += 1
                    trace.mark("bridgeRestart", "#\(restarts): no frontmost app \(Int(Date().timeIntervalSince(since) * 1000)) ms after boot finished")
                    bridgeDoctor.noteRestart(d)
                    restartBridge(d, reason: "readiness: no frontmost app after boot finished (restart #\(restarts))")
                    trace.mark("bridgeRestarted")
                    lastRestartAt = Date()
                }
            }
            sleepMs(100)
        }
        if pid == 0, !isCancelled, d.state == 3, d.lastBootedAt == boot {
            log("\(udid): accessibility did not answer within \(Int(Date().timeIntervalSince(started))) s after boot finished: \(lastError) [timeline: \(trace.summary)]")
        }
        stateLock.lock()
        finished = true
        stateLock.unlock()
        AXWarmup.lock.lock()
        if AXWarmup.running[udid] === self { AXWarmup.running[udid] = nil }
        AXWarmup.lock.unlock()
        done.signal()
    }
}

// Readiness. A booted simulator is
//   - interactive when CoreSimulator's boot status is "finished" (what `simctl bootstatus`
//     waits for), its framebuffer is readable and its HID client is connected: screenshots,
//     taps, typing, launching apps all work;
//   - agent-ready when, in addition, accessibility answers with a frontmost app (the UI tree).
// The steps are independent and run in parallel: framebuffer and HID connect as soon as the
// device is booted, and accessibility is probed (AXWarmup) from the moment boot finished; it is
// not asked earlier, because SpringBoard is still starting (and may be relaunched, e.g. after
// data migration) and early requests only add load.
// waitAccessibility = false returns once interactive and leaves the warm-up running: UI reads
// wait for it. The result carries a timeline of every milestone.
func waitReady(_ d: AGDevice, timeoutMs: Double, waitAccessibility: Bool) throws -> [String: Any] {
    let trace = ReadyTrace()
    let deadline = trace.start.addingTimeInterval(timeoutMs / 1000)
    let bg = DispatchQueue.global(qos: .userInitiated)
    let flagLock = NSLock()
    var connectStarted = false
    var fbError = "", hidError = ""
    var lastState = -1
    var lastBootStatus: Int64 = -2
    var bootedAt: Date?
    var warmup: AXWarmup?
    var lastError = "not booted"
    let boot = d.lastBootedAt
    verifiedLock.lock()
    let verified = boot != nil && verifiedBoots[d.udid] == boot
    verifiedLock.unlock()
    // Only a boot in progress returns before accessibility answers; on a device that was already
    // up, the check is one accessibility request.
    let wasUp = d.state == 3 && ((d.bootStatusInfo?["terminal"] as? Bool) ?? true)
    let returnEarly = !waitAccessibility && !wasUp
    if wasUp, verified, accessibilityAnsweredRecently(d, within: 5), d.framebufferConnected, d.hidConnected {
        return ["readyMs": trace.ms(), "accessibility": "ready", "cached": true, "bridgeRestarts": 0, "device": describe(d)]
    }
    let deferFramebuffer = ProcessInfo.processInfo.environment["AGEMU_IOS_CONNECT_AFTER_BOOT"] != "0"
    var fbStarted = false

    while true {
        let state = Int(d.state)
        if state != lastState {
            trace.mark(state == 3 ? "booted" : "state", d.stateString)
            lastState = state
        }
        if state == 3 {
            if bootedAt == nil { bootedAt = Date() }
            // The framebuffer is connected once boot finished: a framebuffer client makes the
            // simulator render every boot frame for it, and with it connected during boot,
            // boot finished up to 2x later on the loaded test host (docs/IOS.md); after boot
            // the connection takes ~0.1-0.2 s. HID connects at once (no rendering).
            if !fbStarted, !deferFramebuffer || warmup != nil {
                fbStarted = true
                if d.framebufferConnected {
                    trace.mark("framebuffer", "connected")
                } else {
                    bg.async {
                        while Date() < deadline {
                            do {
                                _ = try d.framebuffer()
                                trace.mark("framebuffer")
                                return
                            } catch {
                                flagLock.lock(); fbError = error.localizedDescription; flagLock.unlock()
                            }
                            sleepMs(100)
                        }
                    }
                }
            }
            if !connectStarted {
                connectStarted = true
                if d.hidConnected {
                    trace.mark("hid", "connected")
                } else {
                    bg.async {
                        while Date() < deadline {
                            do {
                                try d.prepareHID()
                                trace.mark("hid")
                                return
                            } catch {
                                flagLock.lock(); hidError = error.localizedDescription; flagLock.unlock()
                            }
                            sleepMs(100)
                        }
                    }
                }
            }
            var finished = true
            if let bs = d.bootStatusInfo {
                let status = (bs["status"] as? NSNumber)?.int64Value ?? -1
                finished = (bs["terminal"] as? Bool) ?? false
                if status != lastBootStatus {
                    trace.mark("bootStatus", bootStatusName(status))
                    lastBootStatus = status
                }
            }
            // Never wait forever on a boot status that does not progress.
            if !finished, let b = bootedAt, Date().timeIntervalSince(b) > 120 { finished = true }
            if warmup == nil && finished {
                trace.mark("bootFinished")
                // The warm-up may outlive this call (waitAccessibility false): give it at
                // least 3 minutes.
                let axDeadline = max(deadline, Date().addingTimeInterval(180))
                warmup = AXWarmup.start(d, trace: trace, full: !verified, deadline: axDeadline)
            }
            let interactive = warmup != nil && trace.has("framebuffer") && trace.has("hid")
            if interactive, let w = warmup, w.isDone || returnEarly {
                if w.isDone && w.pid == 0 {
                    lastError = w.lastError
                } else {
                    trace.mark(w.isDone ? "ready" : "interactive")
                    if w.isDone, let boot {
                        verifiedLock.lock(); verifiedBoots[d.udid] = boot; verifiedLock.unlock()
                    }
                    if !w.isDone {
                        // Log when the UI tree becomes available.
                        let udid = d.udid
                        bg.async {
                            if w.wait(until: Date().addingTimeInterval(200)) {
                                if let boot {
                                    verifiedLock.lock(); verifiedBoots[udid] = boot; verifiedLock.unlock()
                                }
                                log("\(udid): accessibility ready \(Int(trace.at("accessibility") ?? -1)) ms after the ready call (boot finished at \(Int(trace.at("bootFinished") ?? -1)) ms, bridge restarts \(w.restarts))")
                            }
                        }
                    }
                    return [
                        "bootedMs": trace.at("booted") ?? -1,
                        "bootFinishedMs": trace.at("bootFinished") ?? -1,
                        "framebufferMs": trace.at("framebuffer") ?? -1,
                        "hidMs": trace.at("hid") ?? -1,
                        "accessibilityMs": trace.at("accessibility") ?? -1,
                        "readyMs": trace.ms(),
                        "accessibility": w.isDone ? "ready" : "warming",
                        "bridgeRestarts": w.restarts,
                        "frontmostPid": w.pid,
                        "frontmost": w.label,
                        "timeline": trace.timeline,
                        "device": describe(d),
                    ]
                }
            } else if warmup != nil, !(warmup!.isDone) {
                lastError = warmup!.lastError.isEmpty ? "waiting for accessibility" : warmup!.lastError
            }
            // A boot whose display was never created does not recover (SpringBoard keeps
            // crashing): report it so the caller can boot again.
            flagLock.lock()
            let noDisplay = fbError.contains("no display port")
            flagLock.unlock()
            if noDisplay, !trace.has("framebuffer"), let b = bootedAt, Date().timeIntervalSince(b) > 60 {
                throw HelperError("device \(d.udid) came up without a display (no framebuffer 60 s after boot) [timeline: \(trace.summary)]")
            }
        } else {
            lastError = "state \(d.stateString)"
        }
        if Date() >= deadline {
            flagLock.lock()
            var why = lastError
            if state == 3 && !trace.has("framebuffer") { why += "; framebuffer: \(fbError.isEmpty ? "not connected" : fbError)" }
            if state == 3 && !trace.has("hid") { why += "; HID: \(hidError.isEmpty ? "not connected" : hidError)" }
            flagLock.unlock()
            throw HelperError("device \(d.udid) not agent-ready after \(Int(timeoutMs)) ms: \(why) [timeline: \(trace.summary)]")
        }
        // Waiting for boot: poll the (in-process) state every 50 ms; waiting for the
        // connections or accessibility only: every 5 ms.
        sleepMs(warmup == nil ? 50 : 5)
    }
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
        let d = try device(req)
        var out = describe(d)
        if req.optBool("bootStatus"), let bs = d.bootStatusInfo { out["bootStatus"] = bs }
        return out
    case "boot":
        let d = try device(req)
        if d.state != 3 && d.state != 2 { try d.boot() }
        return describe(d)
    case "shutdown":
        let d = try device(req)
        if d.state != 1 { try d.shutdown() }
        return describe(d)
    case "ready":
        return try waitReady(
            try device(req), timeoutMs: req.optDouble("timeoutMs", 120_000),
            waitAccessibility: (req["waitAccessibility"] as? Bool) ?? true)
    case "screenshot":
        let d = try device(req)
        return try Screenshot.capture(
            d, path: try req.string("path"), format: req.optString("format") ?? "png",
            scale: req.optDouble("scale", 1), quality: req.optDouble("quality", 0.85))
    case "tap":
        let d = try device(req)
        let x = try req.double("x"), y = try req.double("y")
        let hold = req.optDouble("holdMs", 0)
        // A tap during a transition (e.g. a navigation pop still sliding in) is ignored by
        // UIKit: wait until the screen has been still for 48 ms first (at most 1 s; instant
        // when it already is).
        let settleMs = tapSettle ? req.optDouble("settleMaxMs", 1000) : 0
        // "ready" is the frame of the element the tap aims at when the caller saw it appear
        // since the last input: wait until it has drawn and stopped moving (waitForRectReady).
        // UIKit drops every touch while a presentation is in progress, and the element is in
        // the UI tree before the presentation's first frame shows.
        var ready: CGRect?
        if tapSettle, let r = req["ready"] as? [Double], r.count == 4 {
            ready = CGRect(x: r[0], y: r[1], width: r[2], height: r[3])
        }
        var readiness: [String: Any] = [:]
        var settle: [String: Any] = [:]
        var shown: [AnyHashable: Any] = [:]
        try inputQueue(d.udid).sync {
            if let r = ready {
                readiness = d.wait(forRectReady: r, quiet: 48, stillQuiet: settleMs > 0 ? 48 : 0, startMax: 500,
                                   tailMax: 300, max: 1500) as? [String: Any] ?? [:]
            }
            if settleMs > 0 { settle = d.wait(forStillQuiet: 48, max: settleMs) as? [String: Any] ?? [:] }
            let mark = d.inputFrameMark()
            try tap(d, x: x, y: y, holdMs: hold)
            // Return once the tap shows on screen (its first frame, at most 120 ms): a check for
            // a still screen right after a tap otherwise saw the old, still screen and typed
            // into a field that was still becoming focused.
            shown = d.waitForChange(since: mark, quiet: 0, max: 120)
        }
        return [
            "ok": true, "settleMs": settle["waitedMs"] ?? 0, "shownMs": shown["waitedMs"] ?? 0,
            "readyMs": readiness["waitedMs"] ?? 0, "pending": readiness["pending"] ?? false,
            "readySettled": readiness["settled"] ?? true,
        ]
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
        // Returns once the app has drawn the text (first new frame, then 48 ms without frames;
        // at most 1 s), so a following tap sees the updated state: a "Send" button enabled by
        // the typed e-mail was still disabled right after an unwaited paste.
        let d = try device(req)
        let text = req.optString("text") ?? ""
        // Like taps, typing waits for a still screen first: a paste sent while a search field
        // was still animating into its focused state was lost. Bounded at 600 ms: a screen
        // with a blinking cursor or a spinner is never still.
        if tapSettle { _ = d.wait(forStillQuiet: 40, max: 600) }
        let mark = try inputQueue(d.udid).sync { try typeText(d, text) }
        let shown = d.waitForChange(since: mark, quiet: 24, max: req.optDouble("settleMaxMs", 500))
        return ["ok": true, "settleMs": shown["waitedMs"] ?? 0]
    case "key":
        let d = try device(req)
        let key = try req.string("key").lowercased()
        if key == "paste" && tapSettle { _ = d.wait(forStillQuiet: 40, max: 600) }
        let mark = key == "paste" ? d.inputFrameMark() : UInt.max
        try inputQueue(d.udid).sync { try pressKey(d, key) }
        if key == "paste" {
            let shown = d.waitForChange(since: mark, quiet: 24, max: req.optDouble("settleMaxMs", 500))
            return ["ok": true, "settleMs": shown["waitedMs"] ?? 0]
        }
        return ["ok": true]
    case "ui":
        let d = try device(req)
        let depth = Int(req.optDouble("maxDepth", -1))
        var tree = try readTree(d, maxDepth: depth, timeout: req.optDouble("timeoutMs", 20_000) / 1000)
        tree["scale"] = d.screenScale
        tree["width"] = Int(d.screenPixels.width)
        tree["height"] = Int(d.screenPixels.height)
        return tree
    case "hit":
        let d = try device(req)
        return try d.accessibility(
            atPointX: try req.double("x"), y: try req.double("y"), maxDepth: Int(req.optDouble("maxDepth", 3)),
            timeout: req.optDouble("timeoutMs", 10_000) / 1000)
    case "ensure-ax":
        return ["switched": try ensureAppAccessibility(try device(req), force: true)]
    case "frontmost":
        let d = try device(req)
        let deadline = Date().addingTimeInterval(req.optDouble("timeoutMs", 5_000) / 1000)
        AXWarmup.waitIfRunning(d, until: deadline)
        do {
            let front = try d.accessibilityFrontmost(
                withTimeout: max(0.1, deadline.timeIntervalSinceNow), full: !req.optBool("pidOnly"))
            bridgeDoctor.noteResult(d, error: nil)
            noteAccessibilityAnswered(d)
            return front
        } catch {
            bridgeDoctor.noteResult(d, error: error)
            throw error
        }
    case "launch":
        let d = try device(req)
        let bundle = try req.string("bundleId")
        if req.optBool("terminateExisting") { try? d.terminate(bundle) }
        d.markInput()
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
    case "nextframe":
        let d = try device(req)
        let since = req.optDouble("since", -1)
        if since < 0 {
            let mark = d.inputFrameMark()
            return ["mark": mark == UInt.max ? 0 : mark]
        }
        return d.waitForFrame(since: UInt(since), quiet: req.optDouble("quietMs", 16), max: req.optDouble("maxMs", 250))
    case "settle":
        let d = try device(req)
        if req.optBool("big") {
            return d.wait(forStillQuiet: req.optDouble("quietMs", 48), max: req.optDouble("maxMs", 1000))
        }
        return d.wait(forSettleQuiet: req.optDouble("quietMs", 48), max: req.optDouble("maxMs", 1000))
    case "notify":
        try device(req).postDarwinNotification(try req.string("name"))
        return ["ok": true]
    default:
        if let out = try handleCapability(cmd, req) { return out }
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

DispatchQueue.global(qos: .utility).async { Screenshot.warmUp() }

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
