// A minimal SwiftUI app used by scripts/e2e-ios.sh to prove that agemu drives third-party
// apps: a button that changes a label, and a text field whose text is echoed.
//
// It is also the probe for the test capabilities (docs/CAPABILITIES.md). The "Capabilities"
// screen (the "Capabilities" button, the -probe launch argument, or any agemutest:// URL) shows
// what the app itself observes:
//   - url: the last URL it was opened with (onOpenURL),
//   - style: the trait collection's light/dark style,
//   - loc: the newest CoreLocation fix (or error) while it is in the foreground,
//   - pb: what UIPasteboard holds (Paste reads it; Copy writes "copied-by-app"),
//   - perm-<name>: the authorization status per privacy service,
//   - push: the last push the notification delegate saw (fg/tap) or the app delegate (bg),
//   - store: data that clear-data must remove (defaults, Documents, Caches, app group) and
//     what it must keep (keychain).
//
// The "Logs" screen (the "Logs" button, or the -logs launch argument) is the probe for logs and
// crashes (docs/CAPABILITIES.md "Logs and crashes"): a line at each unified-log level, a burst
// of numbered lines, and one button per way an app ends (see LogProbe). Launch arguments do the
// same without a tap: -crash <kind> [-crash-delay-ms N], -hang-launch <seconds>.
import AVFoundation
import Contacts
import CoreLocation
import CoreMotion
import EventKit
import MediaPlayer
import Photos
import Security
import SwiftUI
import UIKit
import UserNotifications
import os

let appGroup = "group.dev.agemu.testapp"

@main
struct AgemuTestApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) var delegate
    @StateObject private var probe = Probe.shared

    var body: some Scene {
        WindowGroup {
            Group {
                if probe.showLogs {
                    LogsView()
                } else if probe.showProbe {
                    ProbeView()
                } else {
                    ContentView()
                }
            }
            .environmentObject(probe)
            .onOpenURL { url in
                probe.url = url.absoluteString
                probe.showProbe = true
            }
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication,
                     didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        UNUserNotificationCenter.current().delegate = Probe.shared
        application.registerForRemoteNotifications()
        LogProbe.launched()
        return true
    }

    // Background (content-available) pushes.
    func application(_ application: UIApplication, didReceiveRemoteNotification userInfo: [AnyHashable: Any],
                     fetchCompletionHandler completionHandler: @escaping (UIBackgroundFetchResult) -> Void) {
        DispatchQueue.main.async {
            Probe.shared.push = "bg: " + Probe.describe(userInfo)
            Probe.shared.pushCount += 1
        }
        completionHandler(.noData)
    }
}

final class Probe: NSObject, ObservableObject, CLLocationManagerDelegate, UNUserNotificationCenterDelegate {
    static let shared = Probe()

    @Published var showProbe = ProcessInfo.processInfo.arguments.contains("-probe")
    @Published var showLogs = ProcessInfo.processInfo.arguments.contains("-logs")
    @Published var logStatus = "idle"
    @Published var url = "none"
    @Published var location = "none"
    @Published var pasteboard = "-"
    @Published var pasteboardInfo = ""
    @Published var perms: [(String, String)] = []
    @Published var push = "none"
    @Published var pushCount = 0
    @Published var store = ""

    let lm = CLLocationManager()

    override init() {
        super.init()
        lm.delegate = self
    }

    // MARK: Location

    func locationManagerDidChangeAuthorization(_ m: CLLocationManager) {
        refresh()
        let s = m.authorizationStatus
        if s == .authorizedWhenInUse || s == .authorizedAlways { m.startUpdatingLocation() }
    }

    func locationManager(_ m: CLLocationManager, didUpdateLocations locations: [CLLocation]) {
        if let l = locations.last {
            location = String(format: "%.4f,%.4f", l.coordinate.latitude, l.coordinate.longitude)
        }
    }

    func locationManager(_ m: CLLocationManager, didFailWithError error: Error) {
        location = "error \((error as NSError).code)"
    }

    // Restarts updates, so the current fix (or error) is delivered again.
    func locate() {
        location = "waiting"
        lm.stopUpdatingLocation()
        lm.requestWhenInUseAuthorization()
        lm.startUpdatingLocation()
    }

    // MARK: Notifications

    static func describe(_ userInfo: [AnyHashable: Any]) -> String {
        let aps = userInfo["aps"] as? [String: Any] ?? [:]
        if let alert = aps["alert"] as? [String: Any] {
            return "\(alert["title"] ?? "")|\(alert["body"] ?? "")"
        }
        if let alert = aps["alert"] as? String { return alert }
        if let probe = userInfo["probe"] { return "probe=\(probe)" }
        return "aps=\(aps.keys.sorted().joined(separator: ","))"
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        let info = notification.request.content.userInfo
        DispatchQueue.main.async {
            self.push = "fg: " + Probe.describe(info)
            self.pushCount += 1
        }
        completionHandler([.banner, .list, .sound])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        let info = response.notification.request.content.userInfo
        DispatchQueue.main.async {
            self.push = "tap: " + Probe.describe(info)
            self.showProbe = true
        }
        completionHandler()
    }

    func requestNotifications() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .badge, .sound]) { _, _ in
            DispatchQueue.main.async { self.refresh() }
        }
    }

    // MARK: Pasteboard

    func copy() {
        UIPasteboard.general.string = "copied-by-app"
        refresh()
    }

    func paste() {
        pasteboard = UIPasteboard.general.string ?? "(none)"
        refresh()
    }

    // MARK: Stored data

    var documentsFile: URL { FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0].appendingPathComponent("probe.txt") }
    var cachesFile: URL { FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("probe.txt") }
    var groupFile: URL? {
        FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup)?.appendingPathComponent("probe.txt")
    }
    let keychainQuery: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrAccount as String: "agemu-probe"]

    func save() {
        UserDefaults.standard.set(UserDefaults.standard.integer(forKey: "saved") + 1, forKey: "saved")
        UserDefaults(suiteName: appGroup)?.set("yes", forKey: "saved")
        try? Data("saved".utf8).write(to: documentsFile)
        try? Data("saved".utf8).write(to: cachesFile)
        if let g = groupFile { try? Data("saved".utf8).write(to: g) }
        SecItemDelete(keychainQuery as CFDictionary)
        var add = keychainQuery
        add[kSecValueData as String] = Data("saved".utf8)
        SecItemAdd(add as CFDictionary, nil)
        refresh()
    }

    func storeState() -> String {
        let fm = FileManager.default
        let d = UserDefaults.standard.integer(forKey: "saved")
        let gd = UserDefaults(suiteName: appGroup)?.string(forKey: "saved") ?? "no"
        let gf = groupFile.map { fm.fileExists(atPath: $0.path) ? "yes" : "no" } ?? "nogroup"
        var q = keychainQuery
        q[kSecReturnData as String] = true
        var out: AnyObject?
        let kc = SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess ? "yes" : "no"
        return "defaults=\(d) docs=\(fm.fileExists(atPath: documentsFile.path) ? "yes" : "no") caches=\(fm.fileExists(atPath: cachesFile.path) ? "yes" : "no") groupfile=\(gf) groupdefaults=\(gd) keychain=\(kc)"
    }

    // MARK: Status

    func refresh() {
        let pb = UIPasteboard.general
        pasteboardInfo = "changeCount=\(pb.changeCount) hasStrings=\(pb.hasStrings)"
        store = storeState()
        var p: [(String, String)] = []
        func av(_ s: AVAuthorizationStatus) -> String {
            [.notDetermined: "notDetermined", .restricted: "restricted", .denied: "denied", .authorized: "authorized"][s] ?? "\(s.rawValue)"
        }
        func ph(_ s: PHAuthorizationStatus) -> String {
            [.notDetermined: "notDetermined", .restricted: "restricted", .denied: "denied", .authorized: "authorized", .limited: "limited"][s] ?? "\(s.rawValue)"
        }
        func ek(_ s: EKAuthorizationStatus) -> String {
            [.notDetermined: "notDetermined", .restricted: "restricted", .denied: "denied", .fullAccess: "authorized", .writeOnly: "writeOnly"][s] ?? "\(s.rawValue)"
        }
        p.append(("camera", av(AVCaptureDevice.authorizationStatus(for: .video))))
        p.append(("microphone", av(AVCaptureDevice.authorizationStatus(for: .audio))))
        p.append(("photos", ph(PHPhotoLibrary.authorizationStatus(for: .readWrite))))
        p.append(("photos-add", ph(PHPhotoLibrary.authorizationStatus(for: .addOnly))))
        let loc: [CLAuthorizationStatus: String] = [.notDetermined: "notDetermined", .restricted: "restricted", .denied: "denied",
                                                    .authorizedAlways: "always", .authorizedWhenInUse: "whenInUse"]
        p.append(("location", loc[lm.authorizationStatus] ?? "\(lm.authorizationStatus.rawValue)"))
        // CNAuthorizationStatus.limited (4) is iOS 18+; the app targets iOS 17.
        let cn = ["notDetermined", "restricted", "denied", "authorized", "limited"]
        let cs = CNContactStore.authorizationStatus(for: .contacts).rawValue
        p.append(("contacts", cs < cn.count ? cn[cs] : "\(cs)"))
        p.append(("calendar", ek(EKEventStore.authorizationStatus(for: .event))))
        p.append(("reminders", ek(EKEventStore.authorizationStatus(for: .reminder))))
        let cm: [CMAuthorizationStatus: String] = [.notDetermined: "notDetermined", .restricted: "restricted", .denied: "denied",
                                                   .authorized: "authorized"]
        p.append(("motion", cm[CMMotionActivityManager.authorizationStatus()] ?? "?"))
        let mp: [MPMediaLibraryAuthorizationStatus: String] = [.notDetermined: "notDetermined", .denied: "denied",
                                                               .restricted: "restricted", .authorized: "authorized"]
        p.append(("media-library", mp[MPMediaLibrary.authorizationStatus()] ?? "?"))
        // Siri is not read: INPreferences aborts in an app without the Siri entitlement, and the
        // app has no NSSiriUsageDescription on purpose (a grant must be refused).
        perms = p
        UNUserNotificationCenter.current().getNotificationSettings { s in
            let un: [UNAuthorizationStatus: String] = [.notDetermined: "notDetermined", .denied: "denied", .authorized: "authorized",
                                                       .provisional: "provisional", .ephemeral: "ephemeral"]
            DispatchQueue.main.async {
                self.perms.removeAll { $0.0 == "notifications" }
                self.perms.append(("notifications", un[s.authorizationStatus] ?? "?"))
            }
        }
    }
}

struct ContentView: View {
    @EnvironmentObject var probe: Probe
    @State private var count = 0
    @State private var name = ""

    var body: some View {
        VStack(spacing: 24) {
            Text("agemu test app")
                .font(.title)
                .accessibilityIdentifier("title")
            Text("Count: \(count)")
                .font(.title2)
                .accessibilityIdentifier("count")
            Button("Increment") { count += 1 }
                .buttonStyle(.borderedProminent)
                .accessibilityIdentifier("increment")
            TextField("Your name", text: $name)
                .textFieldStyle(.roundedBorder)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
                .accessibilityIdentifier("name")
                .padding(.horizontal, 32)
            Text(name.isEmpty ? "Hello!" : "Hello, \(name)!")
                .accessibilityIdentifier("greeting")
            Button("Capabilities") { probe.showProbe = true }
                .accessibilityIdentifier("capabilities")
            Button("Logs") { probe.showLogs = true }
                .accessibilityIdentifier("logs")
        }
    }
}

/// The logs and crashes probe. Every line goes to the unified log through os.Logger (subsystem
/// dev.agemu.testapp, category probe) unless its name says otherwise, so agemu's capture can be
/// compared with what the app wrote. Events the measurement scripts need (when the app crashed,
/// how long a burst took) also go to Library/Caches/agemu-probe-events.txt in the app's data
/// container, written before the event, so they do not depend on the capture under test.
enum LogProbe {
    static let logger = Logger(subsystem: "dev.agemu.testapp", category: "probe")
    static let osLog = OSLog(subsystem: "dev.agemu.testapp", category: "probe")
    static let args = ProcessInfo.processInfo.arguments

    static func arg(_ name: String) -> String? {
        guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
        return args[i + 1]
    }

    static var eventsFile: URL {
        FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("agemu-probe-events.txt")
    }

    /// Appends one line to the events file and flushes it to disk (a crash follows some of them).
    static func event(_ line: String) {
        let now = Date().timeIntervalSince1970
        let data = Data(String(format: "%.6f %@\n", now, line).utf8)
        if !FileManager.default.fileExists(atPath: eventsFile.path) {
            FileManager.default.createFile(atPath: eventsFile.path, contents: nil)
        }
        guard let h = try? FileHandle(forWritingTo: eventsFile) else { return }
        defer { try? h.close() }
        _ = try? h.seekToEnd()
        try? h.write(contentsOf: data)
        try? h.synchronize()
    }

    /// Called from didFinishLaunching: a startup line, then the launch-argument triggers.
    static func launched() {
        logger.notice("probe launched pid \(getpid(), privacy: .public)")
        if let s = arg("-hang-launch"), let sec = Double(s) {
            // A main thread blocked during launch: on a device the watchdog kills it.
            event("hang-launch \(sec)")
            Thread.sleep(forTimeInterval: sec)
        }
        if let rate = arg("-log-rate").flatMap(Int.init) { startSteady(rate) }
        if let kind = arg("-crash") {
            let delay = Double(arg("-crash-delay-ms") ?? "0") ?? 0
            DispatchQueue.main.asyncAfter(deadline: .now() + delay / 1000) { crash(kind) }
        }
    }

    /// One line at each level the unified log has, and the other ways an app writes text, all
    /// carrying token so a check can find exactly these.
    static func levels(_ token: String) {
        logger.debug("level debug \(token, privacy: .public)")
        logger.info("level info \(token, privacy: .public)")
        logger.notice("level notice \(token, privacy: .public)")
        logger.warning("level warning \(token, privacy: .public)")
        logger.error("level error \(token, privacy: .public)")
        logger.critical("level critical \(token, privacy: .public)")
        os_log("level oslog-default %{public}@", log: osLog, type: .default, token)
        NSLog("level nslog %@", token)
        print("level print \(token)")
        logger.notice("level multiline \(token, privacy: .public)\nsecond line\nthird line")
    }

    /// Whether the unified log has debug and info turned on for this app's log handle: a
    /// capture must not change them.
    static var enabled: String {
        "debug=\(osLog.isEnabled(type: .debug)) info=\(osLog.isEnabled(type: .info))"
    }

    /// Logs n numbered lines as fast as the app can and returns the time it took (ms).
    @discardableResult
    static func burst(_ n: Int, id: String) -> Double {
        let start = CFAbsoluteTimeGetCurrent()
        for i in 1...n {
            logger.notice("burst \(id, privacy: .public) \(i, privacy: .public)/\(n, privacy: .public)")
        }
        let ms = (CFAbsoluteTimeGetCurrent() - start) * 1000
        event(String(format: "burst %@ n=%d ms=%.3f", id, n, ms))
        return ms
    }

    static var steady: Timer?

    /// Logs rate lines per second (in batches every 50 ms) until stopped (rate 0).
    static func startSteady(_ rate: Int) {
        steady?.invalidate()
        steady = nil
        guard rate > 0 else { return }
        var seq = 0
        let perTick = max(1, rate / 20)
        steady = Timer.scheduledTimer(withTimeInterval: 0.05, repeats: true) { _ in
            for _ in 0..<perTick {
                seq += 1
                logger.notice("steady \(seq, privacy: .public)")
            }
        }
    }

    /// Ends the process the way kind says. The events file gets the time first.
    static func crash(_ kind: String) {
        event("crash \(kind)")
        logger.notice("crash \(kind, privacy: .public) now")
        switch kind {
        case "exception":
            NSException(name: .invalidArgumentException, reason: "agemu probe exception", userInfo: ["probe": kind]).raise()
        case "fatal":
            fatalError("agemu probe fatalError")
        case "badaccess":
            badAccess()
        case "abort":
            abort()
        case "exit":
            exit(0)
        case "sigkill":
            // What a system kill (jetsam on a device) looks like from outside: no crash report.
            kill(getpid(), SIGKILL)
        case "hang":
            // The main thread stops answering; the app is not killed on the Simulator.
            Thread.sleep(forTimeInterval: 30)
        default:
            logger.error("unknown crash kind \(kind, privacy: .public)")
        }
    }

    /// Writes through a null-page pointer: EXC_BAD_ACCESS (SIGSEGV). Not inlined, so the
    /// optimizer keeps the store and the frame names the function.
    @inline(never)
    static func badAccess() {
        let p = UnsafeMutablePointer<Int>(bitPattern: 0x10)!
        p.pointee = 42
    }
}

struct LogsView: View {
    @EnvironmentObject var probe: Probe
    @State private var token = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Logs").font(.headline).accessibilityIdentifier("logs-title")
            Text(probe.logStatus).accessibilityIdentifier("log-status")
            Text("pid \(String(getpid())) \(LogProbe.enabled)").accessibilityIdentifier("log-enabled")
            HStack {
                button("Levels", "log-levels") {
                    token += 1
                    let t = "tok\(getpid())-\(token)"
                    LogProbe.levels(t)
                    probe.logStatus = "levels \(t)"
                }
                button("Burst 1000", "log-burst") {
                    token += 1
                    let id = "b\(getpid())-\(token)"
                    let ms = LogProbe.burst(1000, id: id)
                    probe.logStatus = String(format: "burst %@ n=1000 ms=%.1f", id, ms)
                }
                button("10k", "log-burst-10k") {
                    token += 1
                    let id = "B\(getpid())-\(token)"
                    let ms = LogProbe.burst(10000, id: id)
                    probe.logStatus = String(format: "burst %@ n=10000 ms=%.1f", id, ms)
                }
                button("200/s", "log-steady") {
                    LogProbe.startSteady(LogProbe.steady == nil ? 200 : 0)
                    probe.logStatus = LogProbe.steady == nil ? "steady off" : "steady 200/s"
                }
            }
            HStack {
                button("Exception", "crash-exception") { LogProbe.crash("exception") }
                button("Fatal", "crash-fatal") { LogProbe.crash("fatal") }
                button("Bad access", "crash-badaccess") { LogProbe.crash("badaccess") }
            }
            HStack {
                button("Abort", "crash-abort") { LogProbe.crash("abort") }
                button("Exit", "crash-exit") { LogProbe.crash("exit") }
                button("Kill", "crash-sigkill") { LogProbe.crash("sigkill") }
                button("Hang", "crash-hang") { LogProbe.crash("hang") }
            }
            button("Back", "logs-back") { probe.showLogs = false }
        }
        .font(.caption)
        .padding(.horizontal, 16)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func button(_ title: String, _ id: String, _ action: @escaping () -> Void) -> some View {
        Button(title, action: action).buttonStyle(.bordered).controlSize(.small).accessibilityIdentifier(id)
    }
}

struct ProbeView: View {
    @EnvironmentObject var probe: Probe
    @Environment(\.colorScheme) private var scheme
    @Environment(\.scenePhase) private var phase

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 5) {
                Text("Capabilities").font(.headline).accessibilityIdentifier("probe-title")
                row("url", probe.url)
                row("style", scheme == .dark ? "dark" : "light")
                row("loc", probe.location)
                row("pb", probe.pasteboard)
                row("pbinfo", probe.pasteboardInfo)
                ForEach(probe.perms, id: \.0) { p in
                    Text("\(p.0): \(p.1)").accessibilityIdentifier("perm-\(p.0)")
                }
                row("push", probe.push)
                row("pushes", "\(probe.pushCount)")
                row("store", probe.store)
                HStack {
                    button("Refresh", "refresh") { probe.refresh() }
                    button("Locate", "locate") { probe.locate() }
                    button("Copy", "copy") { probe.copy() }
                    button("Paste", "paste") { probe.paste() }
                }
                HStack {
                    button("Save", "save") { probe.save() }
                    button("Notify", "notify") { probe.requestNotifications() }
                    button("Photos", "ask-photos") {
                        PHPhotoLibrary.requestAuthorization(for: .readWrite) { _ in DispatchQueue.main.async { probe.refresh() } }
                    }
                    button("Camera", "ask-camera") {
                        AVCaptureDevice.requestAccess(for: .video) { _ in DispatchQueue.main.async { probe.refresh() } }
                    }
                    button("Back", "back") { probe.showProbe = false }
                }
            }
            .font(.caption)
            .padding(.horizontal, 16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .onAppear { probe.refresh() }
        .onChange(of: phase) { _, p in if p == .active { probe.refresh() } }
    }

    private func row(_ id: String, _ value: String) -> some View {
        Text("\(id): \(value)").accessibilityIdentifier(id)
    }

    private func button(_ title: String, _ id: String, _ action: @escaping () -> Void) -> some View {
        Button(title, action: action).buttonStyle(.bordered).controlSize(.small).accessibilityIdentifier(id)
    }
}
