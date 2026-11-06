// A minimal SwiftUI app used by scripts/e2e-ios.sh to prove that agemu drives third-party
// apps: a button that changes a label, and a text field whose text is echoed.
import SwiftUI

@main
struct AgemuTestApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
        }
    }
}

struct ContentView: View {
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
        }
    }
}
