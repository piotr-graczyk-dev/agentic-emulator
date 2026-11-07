// Test capabilities (docs/CAPABILITIES.md): open a URL, privacy permissions, simulated location,
// appearance, push, the device pasteboard and app information. Each is one in-process
// CoreSimulator call (AgemuSim's AGCapabilities.m), the call `simctl` would make.
import AgemuSim
import Foundation
import SQLite3

/// Pasteboard items saved by "pasteboard save" (per device), put back by "pasteboard restore".
let savedPasteboardLock = NSLock()
var savedPasteboards: [String: [[String: Data]]] = [:]

func styleName(_ s: Int) -> String {
    switch s {
    case 1: return "light"
    case 2: return "dark"
    default: return "unknown"
    }
}

func readPlist(_ path: String) -> [String: Any]? {
    guard let data = FileManager.default.contents(atPath: path) else { return nil }
    return try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
}

func filePath(_ v: Any?) -> String {
    if let u = v as? URL { return u.path }
    if let s = v as? String { return s.hasPrefix("file://") ? (URL(string: s)?.path ?? s) : s }
    return ""
}

/// One app's information: where it lives, its containers and the Info.plist facts agemu checks
/// (name, versions, the privacy usage descriptions it declares).
func appInfo(_ d: AGDevice, _ bundle: String) -> [String: Any] {
    // CoreSimulator answers an empty dictionary for an app that is not installed.
    guard let props = try? d.applicationProperties(bundle), case let path = filePath(props["Path"] ?? props["Bundle"]),
          !path.isEmpty
    else { return ["id": bundle, "installed": false] }
    var groups: [String: String] = [:]
    for (k, v) in (props["GroupContainers"] as? [String: Any]) ?? [:] { groups[k] = filePath(v) }
    let info = readPlist((path as NSString).appendingPathComponent("Info.plist")) ?? [:]
    let usage = info.keys.filter { $0.hasSuffix("UsageDescription") }.sorted()
    return [
        "id": bundle,
        "installed": true,
        "type": props["ApplicationType"] as? String ?? "",
        "path": path,
        "dataContainer": filePath(props["DataContainer"]),
        "groupContainers": groups,
        "name": info["CFBundleDisplayName"] as? String ?? info["CFBundleName"] as? String ?? "",
        "version": info["CFBundleShortVersionString"] as? String ?? info["CFBundleVersion"] as? String ?? "",
        "usageKeys": usage,
    ]
}

/// User-installed apps read from the device's data directory (milliseconds; see userApps).
func userAppInfos(_ d: AGDevice) -> [[String: Any]] {
    let root = URL(fileURLWithPath: d.dataPath).appendingPathComponent("Containers/Bundle/Application")
    let fm = FileManager.default
    var out: [[String: Any]] = []
    for container in (try? fm.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? [] {
        for item in (try? fm.contentsOfDirectory(at: container, includingPropertiesForKeys: nil)) ?? []
        where item.pathExtension == "app" {
            guard let plist = readPlist(item.appendingPathComponent("Info.plist").path),
                  let bundle = plist["CFBundleIdentifier"] as? String
            else { continue }
            out.append([
                "id": bundle,
                "name": plist["CFBundleDisplayName"] as? String ?? plist["CFBundleName"] as? String ?? "",
                "version": plist["CFBundleShortVersionString"] as? String ?? plist["CFBundleVersion"] as? String ?? "",
                "path": item.path,
            ])
        }
    }
    return out.sorted { ($0["id"] as! String) < ($1["id"] as! String) }
}

/// The device's current privacy grants, read from its own records on the host (read-only):
/// TCC rows (service, client) from data/Library/TCC/TCC.db, and the location clients that have
/// an authorization in locationd's clients.plist. Reset uses it to reset only what was granted:
/// `simctl privacy reset all` without a bundle id changes nothing on iOS 26.5, and resetting
/// every service of every app would also drop the baseline's own system grants.
func privacyGrants(_ d: AGDevice) throws -> [String: Any] {
    var tcc: [[String: String]] = []
    let dbPath = (d.dataPath as NSString).appendingPathComponent("Library/TCC/TCC.db")
    if FileManager.default.fileExists(atPath: dbPath) {
        var db: OpaquePointer?
        guard sqlite3_open_v2(dbPath, &db, SQLITE_OPEN_READONLY, nil) == SQLITE_OK else {
            let msg = db.map { String(cString: sqlite3_errmsg($0)) } ?? "open failed"
            sqlite3_close(db)
            throw HelperError("reading \(dbPath): \(msg)")
        }
        defer { sqlite3_close(db) }
        sqlite3_busy_timeout(db, 2000)
        // client_type: 0 a bundle identifier, 1 an executable path (no bundle to reset by).
        var stmt: OpaquePointer?
        var withType = true
        if sqlite3_prepare_v2(db, "SELECT service, client, client_type FROM access", -1, &stmt, nil) != SQLITE_OK {
            sqlite3_finalize(stmt)
            stmt = nil
            withType = false
            guard sqlite3_prepare_v2(db, "SELECT service, client FROM access", -1, &stmt, nil) == SQLITE_OK else {
                throw HelperError("reading \(dbPath): \(String(cString: sqlite3_errmsg(db)))")
            }
        }
        defer { sqlite3_finalize(stmt) }
        while sqlite3_step(stmt) == SQLITE_ROW {
            guard let s = sqlite3_column_text(stmt, 0), let c = sqlite3_column_text(stmt, 1) else { continue }
            let type = withType ? Int(sqlite3_column_int(stmt, 2)) : 0
            tcc.append(["service": String(cString: s), "client": String(cString: c), "clientType": String(type)])
        }
    }
    var location: [String] = []
    let clients = (d.dataPath as NSString).appendingPathComponent("Library/Caches/locationd/clients.plist")
    for (_, v) in readPlist(clients) ?? [:] {
        guard let c = v as? [String: Any], c["Authorization"] != nil, let bundle = c["BundleId"] as? String else { continue }
        location.append(bundle)
    }
    return ["tcc": tcc, "location": location.sorted()]
}

/// Handles a capability command; nil when cmd is not one.
func handleCapability(_ cmd: String, _ req: [String: Any]) throws -> Any? {
    switch cmd {
    case "openurl":
        try device(req).openURL(try req.string("url"))
        return ["ok": true]
    case "privacy":
        let d = try device(req)
        let service = try req.string("service")
        let bundle = req.optString("bundleId")
        switch try req.string("action") {
        case "grant", "revoke":
            guard let bundle, !bundle.isEmpty else { throw HelperError("privacy grant/revoke needs bundleId") }
            try d.setPrivacy(service, bundleID: bundle, granted: req.optString("action") == "grant")
        case "reset":
            try d.resetPrivacy(service, bundleID: (bundle?.isEmpty ?? true) ? nil : bundle)
        case let a:
            throw HelperError("privacy action must be grant, revoke or reset, got \(a)")
        }
        return ["ok": true]
    case "location":
        let d = try device(req)
        if req.optBool("clear") {
            try d.clearLocation()
        } else {
            try d.setLocationLatitude(try req.double("latitude"), longitude: try req.double("longitude"))
        }
        return ["ok": true]
    case "appearance":
        // Without style: read. With style: set it, and when it changed wait (at most settleMaxMs)
        // for the screen to show it, so a screenshot right after has the new appearance: iOS
        // crossfades the whole screen for ~500 ms, starting 50-100 ms after the call (measured
        // with screenshots every 25 ms; once about 650 ms). So: up to 1000 ms for a big change
        // to begin, then until there has been none for 120 ms. Waiting for the first new frame
        // instead returned before the crossfade began. "redrawn": false says no change was seen.
        let d = try device(req)
        guard let want = req.optString("style"), !want.isEmpty else {
            return ["appearance": styleName(d.interfaceStyle)]
        }
        let target: Int
        switch want {
        case "light": target = 1
        case "dark": target = 2
        default: throw HelperError("appearance must be light or dark, got \(want)")
        }
        if d.interfaceStyle == target {
            return ["appearance": want, "changed": false]
        }
        let maxMs = req.optDouble("settleMaxMs", 0)
        let monitored = maxMs > 0 && d.inputFrameMark() != UInt.max  // starts the display monitor
        try d.setInterfaceStyle(target)
        var shown: [AnyHashable: Any] = [:]
        if monitored {
            shown = d.waitForBigChange(within: min(1000, maxMs), quiet: 120, max: maxMs)
        }
        return ["appearance": styleName(d.interfaceStyle), "changed": true, "redrawn": shown["changed"] ?? false,
                "settleMs": shown["waitedMs"] ?? 0]
    case "push":
        let d = try device(req)
        let bundle = try req.string("bundleId")
        guard let data = try req.string("payload").data(using: .utf8),
              let payload = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        else { throw HelperError("push payload must be a JSON object") }
        try d.sendPush(payload, bundleID: bundle)
        return ["ok": true]
    case "appinfo":
        return appInfo(try device(req), try req.string("bundleId"))
    case "userappinfo":
        return userAppInfos(try device(req))
    case "privacygrants":
        return try privacyGrants(try device(req))
    case "clone":
        // Copies a file tree as an APFS clone (clonefile: no data is copied); a plain copy when
        // cloning is not possible (another volume). clear-data keeps the app bundle this way.
        let src = try req.string("src"), dst = try req.string("dst")
        var cloned = true
        if clonefile(src, dst, 0) != 0 {
            let reason = String(cString: strerror(errno))
            cloned = false
            do {
                try FileManager.default.copyItem(atPath: src, toPath: dst)
            } catch {
                throw HelperError("copying \(src) failed (clonefile: \(reason); copy: \(error.localizedDescription))")
            }
        }
        return ["ok": true, "cloned": cloned]
    case "pasteboard":
        let d = try device(req)
        switch try req.string("op") {
        case "get":
            return ["text": try d.pasteboardString()]
        case "set":
            try d.setPasteboardString(req.optString("text") ?? "")
            return ["ok": true]
        case "save":
            let items = try d.pasteboardItems()
            savedPasteboardLock.lock()
            savedPasteboards[d.udid] = items
            savedPasteboardLock.unlock()
            return ["items": items.count]
        case "restore":
            savedPasteboardLock.lock()
            let items = savedPasteboards.removeValue(forKey: d.udid)
            savedPasteboardLock.unlock()
            guard let items else { throw HelperError("no saved pasteboard for \(d.udid)") }
            try d.setPasteboardItems(items)
            return ["items": items.count]
        case "discard":
            // A paste that could not be confirmed keeps the typed text: drop the saved copy.
            savedPasteboardLock.lock()
            savedPasteboards.removeValue(forKey: d.udid)
            savedPasteboardLock.unlock()
            return ["ok": true]
        case let op:
            throw HelperError("pasteboard op must be get, set, save, restore or discard, got \(op)")
        }
    default:
        return nil
    }
}
