// LeanProbe: app-facing system features that a lean simulator could break. Each button runs one
// API and writes its outcome to the label "result" and to Documents/probe.log (one line per
// outcome, "<epoch> <text>"), so a script can drive it with `agemu tap --id <id>` and read the
// result with `agemu find --id result` or from the app's data container.
// Built by testapps/ios/build-leanprobe.sh. Not used by scripts/e2e-ios.sh.
import ActivityKit
import AlarmKit
import AVFoundation
import Contacts
import CoreSpotlight
import EventKit
import HealthKit
import Intents
import MapKit
import MessageUI
import PassKit
import PhotosUI
import StoreKit
import SwiftUI
import UIKit
import UniformTypeIdentifiers
import UserNotifications
import WatchConnectivity
import WebKit

@main
struct ProbeApp: App {
    init() { UNUserNotificationCenter.current().delegate = NotifDelegate.shared }
    var body: some Scene { WindowGroup { ContentView() } }
}

final class NotifDelegate: NSObject, UNUserNotificationCenterDelegate {
    static let shared = NotifDelegate()
    func userNotificationCenter(_ c: UNUserNotificationCenter, willPresent n: UNNotification,
                                withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
        Log.add("willPresent \(n.request.content.title)")
        done([.banner, .list, .sound])
    }
}

enum Log {
    static func add(_ s: String) {
        let url = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0].appendingPathComponent("probe.log")
        let line = "\(Date().timeIntervalSince1970) \(s)\n"
        if let h = try? FileHandle(forWritingTo: url) { h.seekToEndOfFile(); h.write(line.data(using: .utf8)!); try? h.close() }
        else { try? line.write(to: url, atomically: true, encoding: .utf8) }
    }
}

struct ProbeAttributes: ActivityAttributes {
    struct ContentState: Codable, Hashable { var step: Int }
    var name: String
}

struct ProbeAlarmMeta: AlarmMetadata {}

final class WC: NSObject, WCSessionDelegate {
    var done: ((String) -> Void)?
    func session(_ s: WCSession, activationDidCompleteWith st: WCSessionActivationState, error: Error?) {
        done?("wc state \(st.rawValue) paired \(s.isPaired) err \(error?.localizedDescription ?? "none")")
    }
    func sessionDidBecomeInactive(_ s: WCSession) {}
    func sessionDidDeactivate(_ s: WCSession) {}
}

final class PickerCoordinator: NSObject, UIDocumentPickerDelegate, PHPickerViewControllerDelegate, MFMailComposeViewControllerDelegate, WKNavigationDelegate {
    var done: ((String) -> Void)?
    func documentPicker(_ c: UIDocumentPickerViewController, didPickDocumentsAt urls: [URL]) {
        done?("docpicker picked \(urls.count) \(urls.first?.lastPathComponent ?? "-") at \(urls.first?.deletingLastPathComponent().lastPathComponent ?? "-")")
    }
    func documentPickerWasCancelled(_ c: UIDocumentPickerViewController) { done?("docpicker cancelled") }
    func picker(_ p: PHPickerViewController, didFinishPicking results: [PHPickerResult]) {
        p.dismiss(animated: true)
        guard let r = results.first else { done?("phpicker none"); return }
        r.itemProvider.loadObject(ofClass: UIImage.self) { img, err in
            let i = img as? UIImage
            self.done?("phpicker image \(Int(i?.size.width ?? 0))x\(Int(i?.size.height ?? 0)) err \(err?.localizedDescription ?? "none")")
        }
    }
    func mailComposeController(_ c: MFMailComposeViewController, didFinishWith r: MFMailComposeResult, error: Error?) {
        c.dismiss(animated: true)
        done?("mail result \(r.rawValue) err \(error?.localizedDescription ?? "none")")
    }
    func webView(_ w: WKWebView, didFinish n: WKNavigation!) {
        w.evaluateJavaScript("document.title + ' / ' + document.body.innerText.length") { v, e in
            self.done?("webkit finished \(w.url?.host ?? "local") \(v ?? "-") err \(e?.localizedDescription ?? "none")")
        }
    }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) { done?("webkit fail \(e.localizedDescription)") }
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) { done?("webkit fail \(e.localizedDescription)") }
}

final class Speaker: NSObject, AVSpeechSynthesizerDelegate {
    let synth = AVSpeechSynthesizer()
    var start = Date()
    override init() { super.init(); synth.delegate = self }
    func speak() {
        start = Date()
        synth.speak(AVSpeechUtterance(string: "lean probe"))
    }
    func speechSynthesizer(_ s: AVSpeechSynthesizer, didFinish u: AVSpeechUtterance) {
        Log.add(String(format: "speak finished %.2f s", Date().timeIntervalSince(start)))
    }
}

let speaker = Speaker()

struct Wrap: UIViewControllerRepresentable {
    let make: () -> UIViewController
    func makeUIViewController(context: Context) -> UIViewController { make() }
    func updateUIViewController(_ vc: UIViewController, context: Context) {}
}

struct WebView: UIViewRepresentable {
    let url: URL?
    let coord: PickerCoordinator
    func makeUIView(context: Context) -> WKWebView {
        let w = WKWebView()
        w.navigationDelegate = coord
        if let url { w.load(URLRequest(url: url)) } else {
            w.loadHTMLString("<html><head><title>local</title></head><body><h1>lean probe</h1><script>document.body.append(' js ok')</script></body></html>", baseURL: nil)
        }
        w.accessibilityIdentifier = "web"
        return w
    }
    func updateUIView(_ w: WKWebView, context: Context) {}
}

struct DetectorView: UIViewRepresentable {
    func makeUIView(context: Context) -> UITextView {
        let t = UITextView()
        t.isEditable = false
        t.dataDetectorTypes = .all
        t.font = .systemFont(ofSize: 17)
        t.text = "Call +1 408 555 0100 or mail probe@example.com, visit https://example.com, meet tomorrow at 5 pm at 1 Infinite Loop, Cupertino, CA 95014."
        t.accessibilityIdentifier = "detector"
        return t
    }
    func updateUIView(_ t: UITextView, context: Context) {}
}

let wc = WC()
let coord = PickerCoordinator()
let health = HKHealthStore()

struct ContentView: View {
    @State private var result = "idle"
    @State private var text = ""
    @State private var sheet: String?
    @State private var phItem: PhotosPickerItem?
    @State private var showPhotos = false
    private let contacts = CNContactStore()
    private let events = EKEventStore()

    func set(_ s: String) { DispatchQueue.main.async { result = s; Log.add(s) } }

    func post(_ trigger: UNNotificationTrigger, _ title: String) {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = "local notification"
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: trigger)) { err in
            set("posted \(title) \(err?.localizedDescription ?? "ok")")
        }
    }

    func exportFile() -> URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("leanprobe-\(Int(Date().timeIntervalSince1970)).txt")
        try? "lean probe file".write(to: url, atomically: true, encoding: .utf8)
        return url
    }

    func b(_ title: String, _ id: String, _ action: @escaping () -> Void) -> some View {
        Button(title, action: action).font(.caption2).buttonStyle(.bordered).accessibilityIdentifier(id)
    }

    var body: some View {
        VStack(spacing: 6) {
            Text(result).accessibilityIdentifier("result").font(.caption).lineLimit(3)
            TextField("Type here", text: $text).textFieldStyle(.roundedBorder).accessibilityIdentifier("field")
            Text("typed: \(text)").font(.caption2).accessibilityIdentifier("typed")
            ScrollView {
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 100), spacing: 4)], spacing: 4) {
                    // Notifications and Focus
                    b("Notify auth", "notify") {
                        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .badge, .sound]) { ok, err in
                            UNUserNotificationCenter.current().getNotificationSettings { s in
                                set("notify \(ok) auth \(s.authorizationStatus.rawValue) alert \(s.alertSetting.rawValue) \(err?.localizedDescription ?? "")")
                            }
                        }
                    }
                    b("Post now", "postnow") { post(UNTimeIntervalNotificationTrigger(timeInterval: 1, repeats: false), "probe now") }
                    b("Post 20s", "post20") { post(UNTimeIntervalNotificationTrigger(timeInterval: 20, repeats: false), "probe 20s") }
                    b("Post cal 60s", "postcal") {
                        let d = Calendar.current.dateComponents([.year, .month, .day, .hour, .minute, .second], from: Date().addingTimeInterval(60))
                        post(UNCalendarNotificationTrigger(dateMatching: d, repeats: false), "probe cal")
                    }
                    b("Post 150s", "post150") { post(UNTimeIntervalNotificationTrigger(timeInterval: 150, repeats: false), "probe 150s") }
                    b("Pending", "pending") {
                        UNUserNotificationCenter.current().getPendingNotificationRequests { rs in set("pending \(rs.count)") }
                    }
                    b("Delivered", "delivered") {
                        UNUserNotificationCenter.current().getDeliveredNotifications { ns in
                            set("delivered \(ns.count): \(Dictionary(grouping: ns.map { $0.request.content.title }, by: { $0 }).map { "\($0.key)x\($0.value.count)" }.sorted().joined(separator: ","))")
                        }
                    }
                    b("Focus", "focus") {
                        INFocusStatusCenter.default.requestAuthorization { st in
                            set("focus auth \(st.rawValue) focused \(String(describing: INFocusStatusCenter.default.focusStatus.isFocused))")
                        }
                    }
                    // Files
                    b("Doc open", "docopen") { sheet = "docopen" }
                    b("Doc export", "docexport") { sheet = "docexport" }
                    b("Share file", "sharefile") { sheet = "sharefile" }
                    b("Share text", "share") { sheet = "share" }
                    // Mail
                    b("Mail compose", "mail") {
                        if MFMailComposeViewController.canSendMail() { sheet = "mail" } else { set("mail canSendMail false") }
                    }
                    b("mailto", "mailto") {
                        UIApplication.shared.open(URL(string: "mailto:probe@example.com?subject=lean")!) { ok in set("mailto opened \(ok)") }
                    }
                    // Health
                    b("Health auth", "healthauth") {
                        guard HKHealthStore.isHealthDataAvailable() else { set("health unavailable"); return }
                        let steps = HKQuantityType(.stepCount)
                        health.requestAuthorization(toShare: [steps], read: [steps]) { ok, err in
                            set("health auth \(ok) share \(health.authorizationStatus(for: steps).rawValue) err \(err?.localizedDescription ?? "none")")
                        }
                    }
                    b("Health RW", "healthrw") {
                        let steps = HKQuantityType(.stepCount)
                        let now = Date()
                        let s = HKQuantitySample(type: steps, quantity: HKQuantity(unit: .count(), doubleValue: 123), start: now.addingTimeInterval(-60), end: now)
                        health.save(s) { ok, err in
                            guard ok else { set("health save failed \(err?.localizedDescription ?? "")"); return }
                            let q = HKSampleQuery(sampleType: steps, predicate: HKQuery.predicateForSamples(withStart: now.addingTimeInterval(-3600), end: nil), limit: 100, sortDescriptors: nil) { _, rs, e in
                                let total = (rs as? [HKQuantitySample])?.reduce(0) { $0 + $1.quantity.doubleValue(for: .count()) } ?? -1
                                set("health save ok read \(rs?.count ?? -1) total \(Int(total)) err \(e?.localizedDescription ?? "none")")
                            }
                            health.execute(q)
                        }
                    }
                    // Spotlight
                    b("Spotlight", "spotlight") {
                        let tag = "leanprobe\(Int.random(in: 10000...99999))"
                        let attrs = CSSearchableItemAttributeSet(contentType: .text)
                        attrs.title = "Lean probe \(tag)"
                        attrs.contentDescription = "indexed by lean probe"
                        let item = CSSearchableItem(uniqueIdentifier: tag, domainIdentifier: "probe", attributeSet: attrs)
                        CSSearchableIndex.default().indexSearchableItems([item]) { err in
                            if let err { set("spotlight index error \(err.localizedDescription)"); return }
                            DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
                                let ctx = CSSearchQueryContext()
                                ctx.fetchAttributes = ["title"]
                                let q = CSSearchQuery(queryString: "title == \"*\(tag)*\"", queryContext: ctx)
                                var found = 0
                                q.foundItemsHandler = { found += $0.count }
                                q.completionHandler = { e in set("spotlight indexed \(tag) found \(found) err \(e?.localizedDescription ?? "none")") }
                                q.start()
                            }
                        }
                    }
                    // Text, photos, web, maps
                    b("Detectors", "detectors") { sheet = "detectors" }
                    b("NSDataDetector", "nsdd") {
                        let s = "Call +1 408 555 0100, mail probe@example.com, https://example.com, tomorrow at 5 pm, 1 Infinite Loop, Cupertino, CA 95014"
                        let d = try! NSDataDetector(types: NSTextCheckingAllTypes)
                        let m = d.matches(in: s, range: NSRange(s.startIndex..., in: s))
                        set("nsdd \(m.count) types \(m.map { $0.resultType.rawValue }.map(String.init).joined(separator: ","))")
                    }
                    b("PHPicker", "phpicker") { sheet = "phpicker" }
                    PhotosPicker("PhotosPicker", selection: $phItem, matching: .images).font(.caption2).buttonStyle(.bordered).accessibilityIdentifier("photospicker")
                        .onChange(of: phItem) { _, it in
                            guard let it else { return }
                            Task {
                                let d = try? await it.loadTransferable(type: Data.self)
                                set("photospicker data \(d?.count ?? -1)")
                            }
                        }
                    b("Web remote", "webremote") { sheet = "webremote" }
                    b("Web local", "weblocal") { sheet = "weblocal" }
                    b("Map search", "mapsearch") {
                        let q = MKLocalSearch.Request()
                        q.naturalLanguageQuery = "coffee"
                        q.region = MKCoordinateRegion(center: CLLocationCoordinate2D(latitude: 37.3349, longitude: -122.0090), latitudinalMeters: 5000, longitudinalMeters: 5000)
                        MKLocalSearch(request: q).start { r, e in
                            set("mapsearch \(r?.mapItems.count ?? -1) first \(r?.mapItems.first?.name ?? "-") err \(e?.localizedDescription ?? "none")")
                        }
                    }
                    b("Directions", "directions") {
                        let q = MKDirections.Request()
                        q.source = MKMapItem(placemark: MKPlacemark(coordinate: CLLocationCoordinate2D(latitude: 37.3349, longitude: -122.0090)))
                        q.destination = MKMapItem(placemark: MKPlacemark(coordinate: CLLocationCoordinate2D(latitude: 37.7793, longitude: -122.4193)))
                        MKDirections(request: q).calculate { r, e in
                            set("directions routes \(r?.routes.count ?? -1) km \(Int((r?.routes.first?.distance ?? 0) / 1000)) err \(e?.localizedDescription ?? "none")")
                        }
                    }
                    b("Map view", "mapview") { sheet = "map" }
                    // Live Activities, alarms, timers
                    b("Live activity", "liveact") {
                        let info = ActivityAuthorizationInfo()
                        do {
                            let a = try Activity.request(attributes: ProbeAttributes(name: "probe"), content: .init(state: .init(step: 1), staleDate: nil))
                            Task {
                                await a.update(.init(state: .init(step: 2), staleDate: nil))
                                set("liveact enabled \(info.areActivitiesEnabled) id \(a.id.prefix(8)) state \(a.activityState) count \(Activity<ProbeAttributes>.activities.count)")
                            }
                        } catch { set("liveact enabled \(info.areActivitiesEnabled) error \(error.localizedDescription)") }
                    }
                    b("Activity state", "livestate") {
                        let acts = Activity<ProbeAttributes>.activities
                        set("livestate count \(acts.count) states \(acts.map { "\($0.activityState)" }.joined(separator: ","))")
                    }
                    b("End activities", "liveend") {
                        Task {
                            for a in Activity<ProbeAttributes>.activities { await a.end(nil, dismissalPolicy: .immediate) }
                            set("liveact ended, left \(Activity<ProbeAttributes>.activities.filter { $0.activityState == .active }.count)")
                        }
                    }
                    b("Alarm", "alarm") {
                        Task {
                            do {
                                let st = try await AlarmManager.shared.requestAuthorization()
                                let alert = AlarmPresentation.Alert(title: "Probe alarm", stopButton: AlarmButton(text: "Stop", textColor: .white, systemImageName: "stop"))
                                let attrs = AlarmAttributes<ProbeAlarmMeta>(presentation: AlarmPresentation(alert: alert), tintColor: .blue)
                                let id = UUID()
                                let cfg = AlarmManager.AlarmConfiguration<ProbeAlarmMeta>.timer(duration: 30, attributes: attrs)
                                let a = try await AlarmManager.shared.schedule(id: id, configuration: cfg)
                                let all = try AlarmManager.shared.alarms
                                set("alarm auth \(st) scheduled \(a.state) count \(all.count)")
                            } catch { set("alarm error \(error.localizedDescription)") }
                        }
                    }
                    b("Alarm 180s", "alarm180") {
                        Task {
                            do {
                                _ = try await AlarmManager.shared.requestAuthorization()
                                let alert = AlarmPresentation.Alert(title: "Probe alarm 180", stopButton: AlarmButton(text: "Stop", textColor: .white, systemImageName: "stop"))
                                let attrs = AlarmAttributes<ProbeAlarmMeta>(presentation: AlarmPresentation(alert: alert), tintColor: .blue)
                                let a = try await AlarmManager.shared.schedule(id: UUID(), configuration: .timer(duration: 180, attributes: attrs))
                                set("alarm180 scheduled \(a.state)")
                                Task {
                                    for await all in AlarmManager.shared.alarmUpdates {
                                        Log.add("alarm update \(all.map { "\($0.state)" }.joined(separator: ","))")
                                    }
                                }
                            } catch { set("alarm180 error \(error.localizedDescription)") }
                        }
                    }
                    b("Stop alarms", "alarmstop") {
                        do {
                            let all = try AlarmManager.shared.alarms
                            for a in all { try? AlarmManager.shared.stop(id: a.id); try? AlarmManager.shared.cancel(id: a.id) }
                            set("alarmstop \(all.count) left \((try? AlarmManager.shared.alarms.count) ?? -1)")
                        } catch { set("alarmstop error \(error.localizedDescription)") }
                    }
                    b("Alarms", "alarms") {
                        do { let all = try AlarmManager.shared.alarms; set("alarms \(all.count) states \(all.map { "\($0.state)" }.joined(separator: ","))") }
                        catch { set("alarms error \(error.localizedDescription)") }
                    }
                    // Contacts, calendar, reminders
                    b("Contacts RW", "contacts") {
                        contacts.requestAccess(for: .contacts) { ok, err in
                            guard ok else { set("contacts denied \(err?.localizedDescription ?? "")"); return }
                            let c = CNMutableContact()
                            c.givenName = "Lean"
                            c.familyName = "Probe\(Int.random(in: 1000...9999))"
                            let save = CNSaveRequest()
                            save.add(c, toContainerWithIdentifier: nil)
                            do {
                                try contacts.execute(save)
                                let found = try contacts.unifiedContacts(matching: CNContact.predicateForContacts(matchingName: c.familyName), keysToFetch: [CNContactGivenNameKey as CNKeyDescriptor])
                                set("contacts write ok read \(found.count)")
                            } catch { set("contacts error \(error.localizedDescription)") }
                        }
                    }
                    b("Calendar RW", "calendar") {
                        events.requestFullAccessToEvents { ok, err in
                            guard ok else { set("calendar denied \(err?.localizedDescription ?? "")"); return }
                            let e = EKEvent(eventStore: events)
                            e.title = "lean probe event"
                            e.startDate = Date().addingTimeInterval(3600)
                            e.endDate = Date().addingTimeInterval(7200)
                            e.calendar = events.defaultCalendarForNewEvents
                            do {
                                try events.save(e, span: .thisEvent)
                                let p = events.predicateForEvents(withStart: Date(), end: Date().addingTimeInterval(86400), calendars: nil)
                                set("calendar write ok read \(events.events(matching: p).filter { $0.title == "lean probe event" }.count)")
                            } catch { set("calendar error \(error.localizedDescription)") }
                        }
                    }
                    b("Reminders RW", "reminders") {
                        events.requestFullAccessToReminders { ok, err in
                            guard ok else { set("reminders denied \(err?.localizedDescription ?? "")"); return }
                            let r = EKReminder(eventStore: events)
                            r.title = "lean probe reminder"
                            r.calendar = events.defaultCalendarForNewReminders()
                            do {
                                try events.save(r, commit: true)
                                events.fetchReminders(matching: events.predicateForReminders(in: nil)) { rs in
                                    set("reminders write ok read \(rs?.filter { $0.title == "lean probe reminder" }.count ?? -1)")
                                }
                            } catch { set("reminders error \(error.localizedDescription)") }
                        }
                    }
                    // Misc services
                    b("PassKit", "passkit") {
                        set("passkit lib \(PKPassLibrary.isPassLibraryAvailable()) canPay \(PKPaymentAuthorizationController.canMakePayments()) passes \(PKPassLibrary().passes().count)")
                    }
                    b("StoreKit", "storekit") {
                        Task {
                            do {
                                let p = try await Product.products(for: ["dev.agemu.probe.coin"])
                                set("storekit products \(p.count) canPay \(AppStore.canMakePayments)")
                            } catch { set("storekit error \(error.localizedDescription)") }
                        }
                    }
                    b("Watch", "watch") {
                        guard WCSession.isSupported() else { set("wc unsupported"); return }
                        wc.done = { s in set(s) }
                        WCSession.default.delegate = wc
                        WCSession.default.activate()
                    }
                    b("Network", "network") {
                        URLSession.shared.dataTask(with: URL(string: "https://example.com")!) { d, r, e in
                            set("network status \((r as? HTTPURLResponse)?.statusCode ?? -1) bytes \(d?.count ?? -1) err \(e?.localizedDescription ?? "none")")
                        }.resume()
                    }
                    b("Speech", "speech") {
                        let v = AVSpeechSynthesisVoice.speechVoices()
                        set("speech voices \(v.count)")
                    }
                    b("Speak", "speak") { speaker.speak() }
                    b("Input modes", "modes") {
                        set("modes " + UITextInputMode.activeInputModes.map { $0.primaryLanguage ?? "nil" }.joined(separator: ","))
                    }
                }.padding(.horizontal, 6)
            }
        }
        .padding(.top, 4)
        .onAppear { coord.done = { s in set(s) } }
        .sheet(item: Binding(get: { sheet.map { S(id: $0) } }, set: { sheet = $0?.id })) { s in
            switch s.id {
            case "share": Wrap { UIActivityViewController(activityItems: ["lean probe"], applicationActivities: nil) }
            case "sharefile": Wrap { UIActivityViewController(activityItems: [exportFile()], applicationActivities: nil) }
            case "docopen": Wrap {
                let p = UIDocumentPickerViewController(forOpeningContentTypes: [.item])
                p.delegate = coord
                return p
            }
            case "docexport": Wrap {
                let p = UIDocumentPickerViewController(forExporting: [exportFile()], asCopy: true)
                p.delegate = coord
                return p
            }
            case "mail": Wrap {
                let m = MFMailComposeViewController()
                m.mailComposeDelegate = coord
                m.setToRecipients(["probe@example.com"])
                m.setSubject("lean probe")
                return m
            }
            case "phpicker": Wrap {
                var c = PHPickerConfiguration()
                c.filter = .images
                let p = PHPickerViewController(configuration: c)
                p.delegate = coord
                return p
            }
            case "detectors": DetectorView().frame(height: 300)
            case "webremote": WebView(url: URL(string: "https://example.com"), coord: coord)
            case "weblocal": WebView(url: nil, coord: coord)
            default: Map(initialPosition: .region(MKCoordinateRegion(center: CLLocationCoordinate2D(latitude: 37.3349, longitude: -122.0090), latitudinalMeters: 3000, longitudinalMeters: 3000)))
                .accessibilityIdentifier("map")
            }
        }
    }
}

struct S: Identifiable { let id: String }
