// Agemu Shop for iOS: the same store as the Android app (examples/shop), in SwiftUI. Sign in, a
// catalog of 36 products that scrolls past the screen, a search field with a 300 ms debounce, a
// product page with a quantity stepper, a cart, a checkout form and a confirmation. Sign-in and
// payment wait 700 and 900 ms. State is in memory: a relaunch starts signed out with an empty cart.
// Built by build.sh with swiftc (no Xcode project).
import SwiftUI

struct Product: Hashable {
    let name: String
    let category: String
    let cents: Int
    let blurb: String
}

let products: [Product] = [
    Product(name: "Desk Lamp", category: "Lighting", cents: 2999, blurb: "Warm 2700 K light on a weighted steel base. Tilts 90 degrees."),
    Product(name: "Floor Lamp", category: "Lighting", cents: 8900, blurb: "A linen shade on a slim oak stand, 160 cm tall."),
    Product(name: "Ceramic Mug", category: "Kitchen", cents: 1250, blurb: "Hand-glazed stoneware, 350 ml. Dishwasher safe."),
    Product(name: "Pour-Over Kettle", category: "Kitchen", cents: 5400, blurb: "Gooseneck spout for a slow, even pour. 1 litre."),
    Product(name: "Linen Napkins", category: "Dining", cents: 1800, blurb: "Set of four, stonewashed for softness."),
    Product(name: "Oak Cutting Board", category: "Kitchen", cents: 3500, blurb: "End-grain oak, oiled by hand, 40 x 25 cm."),
    Product(name: "Cast Iron Pan", category: "Kitchen", cents: 4200, blurb: "Pre-seasoned, 26 cm. Goes from stove to oven."),
    Product(name: "Glass Carafe", category: "Dining", cents: 2200, blurb: "Borosilicate glass with a cork stopper, 1 litre."),
    Product(name: "Throw Pillow", category: "Living", cents: 2700, blurb: "Washed cotton cover with a feather insert."),
    Product(name: "Scented Candle", category: "Living", cents: 1600, blurb: "Cedar and fig, soy wax, 40 hours of burn time."),
    Product(name: "Plant Pot", category: "Garden", cents: 1900, blurb: "Terracotta with a drainage saucer, 18 cm."),
    Product(name: "Wall Clock", category: "Living", cents: 3900, blurb: "Silent sweep movement in a birch frame."),
    Product(name: "Bookends", category: "Office", cents: 2400, blurb: "A pair of powder-coated steel bookends."),
    Product(name: "Desk Organizer", category: "Office", cents: 2100, blurb: "Walnut tray with three compartments."),
    Product(name: "Notebook Set", category: "Office", cents: 1400, blurb: "Three dot-grid notebooks, A5, lay-flat binding."),
    Product(name: "Fountain Pen", category: "Office", cents: 4800, blurb: "Steel nib, brass body, refillable converter."),
    Product(name: "Reading Glasses", category: "Accessories", cents: 2500, blurb: "Light acetate frames, +1.5 lenses."),
    Product(name: "Bath Towel", category: "Bath", cents: 3200, blurb: "Long-staple cotton, 600 g/m2, 70 x 140 cm."),
    Product(name: "Shower Caddy", category: "Bath", cents: 2800, blurb: "Rust-proof stainless steel, hangs over the head."),
    Product(name: "Door Mat", category: "Home", cents: 2300, blurb: "Coir fibre on a rubber backing, 60 x 40 cm."),
    Product(name: "Umbrella", category: "Accessories", cents: 3100, blurb: "Wind-resistant frame, opens with one button."),
    Product(name: "Travel Mug", category: "Kitchen", cents: 2600, blurb: "Vacuum insulated, keeps coffee hot for 6 hours."),
    Product(name: "Wool Blanket", category: "Living", cents: 7900, blurb: "Merino wool throw, 130 x 180 cm, woven in Portugal."),
    Product(name: "Picnic Basket", category: "Garden", cents: 5800, blurb: "Willow basket with plates and cutlery for four."),
    Product(name: "Linen Duvet", category: "Bedroom", cents: 12900, blurb: "Stonewashed linen, double size, with two pillowcases."),
    Product(name: "Reading Light", category: "Lighting", cents: 3400, blurb: "A small ceramic lamp with a dimmer switch."),
    Product(name: "Wool Rug", category: "Living", cents: 14500, blurb: "Hand-tufted wool, 160 x 230 cm."),
    Product(name: "Coat Hooks", category: "Home", cents: 1700, blurb: "Three solid ash hooks on a brass rail."),
    Product(name: "Tea Towels", category: "Kitchen", cents: 1500, blurb: "Set of three, cotton herringbone."),
    Product(name: "Salad Bowl", category: "Dining", cents: 3800, blurb: "Turned acacia wood, 28 cm."),
    Product(name: "Water Glasses", category: "Dining", cents: 2600, blurb: "Set of four, mouth-blown glass."),
    Product(name: "Desk Chair", category: "Office", cents: 18900, blurb: "Oak frame, woven paper-cord seat."),
    Product(name: "Watering Can", category: "Garden", cents: 2900, blurb: "Galvanised steel, 2 litres, long spout."),
    Product(name: "Garden Gloves", category: "Garden", cents: 1300, blurb: "Goatskin palms, cotton backs."),
    Product(name: "Bath Mat", category: "Bath", cents: 2100, blurb: "Cotton waffle weave, 50 x 80 cm."),
    Product(name: "Soap Dish", category: "Bath", cents: 900, blurb: "Unglazed stoneware that drains.")
]

func money(_ cents: Int) -> String { String(format: "$%d.%02d", cents / 100, cents % 100) }

enum Route: Hashable {
    case product(Int)
    case cart
    case checkout
    case confirmation(order: Int, items: Int, total: Int, name: String)
}

@MainActor
final class Store: ObservableObject {
    @Published var signedIn = false
    @Published var path: [Route] = []
    @Published var cart: [(product: Int, qty: Int)] = []

    var count: Int { cart.reduce(0) { $0 + $1.qty } }
    var total: Int { cart.reduce(0) { $0 + products[$1.product].cents * $1.qty } }

    func add(_ product: Int, _ qty: Int) {
        if let i = cart.firstIndex(where: { $0.product == product }) {
            cart[i].qty += qty
        } else {
            cart.append((product, qty))
        }
    }
}

extension Color {
    static let bg = Color(red: 0.965, green: 0.957, blue: 0.937)
    static let ink = Color(red: 0.114, green: 0.106, blue: 0.094)
    static let muted = Color(red: 0.435, green: 0.416, blue: 0.380)
    static let accent = Color(red: 0.851, green: 0.310, blue: 0.169)
    static let line = Color(red: 0.894, green: 0.878, blue: 0.843)
}

struct Card<Content: View>: View {
    @ViewBuilder var content: Content
    var body: some View {
        content
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.white)
            .overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.line))
            .clipShape(RoundedRectangle(cornerRadius: 14))
    }
}

struct Field: View {
    let title: String
    @Binding var text: String
    var secure = false
    var body: some View {
        Group {
            if secure {
                SecureField(title, text: $text)
            } else {
                TextField(title, text: $text)
            }
        }
        .accessibilityLabel(title)
        .textInputAutocapitalization(.never)
        .autocorrectionDisabled()
        .font(.system(size: 17))
        .padding(14)
        .background(Color.white)
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.line))
    }
}

struct Primary: View {
    let title: String
    var disabled = false
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Text(title).font(.system(size: 17, weight: .semibold)).foregroundColor(.white)
                .frame(maxWidth: .infinity).padding(.vertical, 15)
                .background(Color.accent.opacity(disabled ? 0.5 : 1))
                .clipShape(RoundedRectangle(cornerRadius: 14))
        }
        .disabled(disabled)
    }
}

struct Secondary: View {
    let title: String
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Text(title).font(.system(size: 16)).foregroundColor(.ink)
                .padding(.horizontal, 16).padding(.vertical, 10)
                .background(Color.white)
                .overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.line))
        }
    }
}

struct Title: View {
    let text: String
    var body: some View {
        Text(text).font(.system(size: 30, weight: .semibold)).foregroundColor(.ink)
            .accessibilityAddTraits(.isHeader)
    }
}

@main
struct ShopApp: App {
    @StateObject private var store = Store()
    var body: some Scene {
        WindowGroup {
            Group {
                if store.signedIn {
                    NavigationStack(path: $store.path) {
                        CatalogView()
                            .navigationDestination(for: Route.self) { route in
                                switch route {
                                case .product(let i): ProductView(index: i)
                                case .cart: CartView()
                                case .checkout: CheckoutView()
                                case let .confirmation(order, items, total, name):
                                    ConfirmationView(order: order, items: items, total: total, name: name)
                                }
                            }
                    }
                } else {
                    LoginView()
                }
            }
            .environmentObject(store)
            .preferredColorScheme(.light)
            .tint(.accent)
        }
    }
}

struct Screen<Content: View>: View {
    @ViewBuilder var content: Content
    var body: some View {
        ZStack(alignment: .topLeading) {
            Color.bg.ignoresSafeArea()
            VStack(alignment: .leading, spacing: 0) { content }
                .padding(.horizontal, 20)
                .padding(.top, 8)
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
        .toolbar(.hidden, for: .navigationBar)
        .navigationBarBackButtonHidden(true)
    }
}

struct LoginView: View {
    @EnvironmentObject var store: Store
    @State private var email = ""
    @State private var password = ""
    @State private var status = ""
    @State private var busy = false
    var body: some View {
        Screen {
            Title(text: "Agemu Shop").padding(.top, 12)
            Text("Sign in to continue").font(.system(size: 17)).foregroundColor(.muted).padding(.top, 4)
            Field(title: "E-mail", text: $email).keyboardType(.emailAddress).padding(.top, 32)
            Field(title: "Password", text: $password, secure: true).padding(.top, 12)
            if !status.isEmpty {
                Text(status).font(.system(size: 15)).foregroundColor(.accent).padding(.top, 12)
            }
            Primary(title: "Sign in", disabled: busy) {
                guard email.trimmingCharacters(in: .whitespaces) == "test@agemu.dev", password == "hunter2" else {
                    status = "Wrong e-mail or password"
                    return
                }
                busy = true
                status = "Signing in…"
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.7) { store.signedIn = true }
            }
            .padding(.top, 12)
            Text("Demo account: test@agemu.dev / hunter2").font(.system(size: 14)).foregroundColor(.muted).padding(.top, 24)
        }
    }
}

struct CatalogView: View {
    @EnvironmentObject var store: Store
    @State private var search = ""
    @State private var query = ""
    @State private var debounce: Task<Void, Never>?

    var shown: [Int] {
        products.indices.filter { i in
            query.isEmpty || products[i].name.lowercased().contains(query) || products[i].category.lowercased().contains(query)
        }
    }

    var body: some View {
        Screen {
            HStack {
                Title(text: "Catalog")
                Spacer()
                Secondary(title: "Cart (\(store.count))") { store.path.append(.cart) }
            }
            .padding(.top, 12)
            Field(title: "Search products", text: $search).padding(.top, 12)
                .onChange(of: search) { _, value in
                    debounce?.cancel()
                    debounce = Task {
                        try? await Task.sleep(nanoseconds: 300_000_000)
                        if !Task.isCancelled { query = value.trimmingCharacters(in: .whitespaces).lowercased() }
                    }
                }
            let list = shown
            Text(list.count == 1 ? "1 product" : "\(list.count) products").font(.system(size: 14)).foregroundColor(.muted).padding(.top, 8)
            ScrollView {
                LazyVStack(spacing: 10) {
                    ForEach(list, id: \.self) { i in
                        Card {
                            HStack {
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(products[i].name).font(.system(size: 18)).foregroundColor(.ink)
                                    Text(products[i].category).font(.system(size: 14)).foregroundColor(.muted)
                                }
                                Spacer()
                                Text(money(products[i].cents)).font(.system(size: 17)).foregroundColor(.ink)
                            }
                        }
                        .contentShape(Rectangle())
                        .onTapGesture { store.path.append(.product(i)) }
                    }
                    if list.isEmpty { Text("No products match").foregroundColor(.muted) }
                }
                .padding(.vertical, 4)
            }
            .scrollDismissesKeyboard(.immediately)
            .padding(.top, 8)
        }
    }
}

struct ProductView: View {
    @EnvironmentObject var store: Store
    let index: Int
    @State private var qty = 1
    @State private var added = ""
    var body: some View {
        let p = products[index]
        Screen {
            Secondary(title: "‹ Catalog") { store.path.removeLast() }
            Title(text: p.name).padding(.top, 16)
            Text(p.category).font(.system(size: 15)).foregroundColor(.muted)
            Text(money(p.cents)).font(.system(size: 26)).foregroundColor(.accent).padding(.top, 16)
            Text(p.blurb).font(.system(size: 17)).foregroundColor(.ink).padding(.top, 12)
            HStack {
                Text("Quantity: \(qty)").font(.system(size: 17)).foregroundColor(.ink)
                Spacer()
                Secondary(title: "−") { qty = max(1, qty - 1) }
                Secondary(title: "+") { qty = min(9, qty + 1) }
            }
            .padding(.top, 28)
            Primary(title: "Add to cart") {
                store.add(index, qty)
                added = "Added \(qty) × \(p.name) · cart: \(store.count)"
            }
            .padding(.top, 28)
            if !added.isEmpty {
                Text(added).font(.system(size: 16)).foregroundColor(.muted).padding(.top, 12)
            }
            Button { store.path.append(.cart) } label: {
                Text("View cart").font(.system(size: 16)).foregroundColor(.ink)
                    .frame(maxWidth: .infinity).padding(.vertical, 13)
                    .background(Color.white).overlay(RoundedRectangle(cornerRadius: 14).stroke(Color.line))
            }
            .padding(.top, 12)
        }
    }
}

struct CartView: View {
    @EnvironmentObject var store: Store
    var body: some View {
        Screen {
            Secondary(title: "‹ Back") { store.path.removeLast() }
            Title(text: "Your cart").padding(.top, 16)
            if store.cart.isEmpty {
                Text("Your cart is empty").foregroundColor(.muted).padding(.top, 12)
            } else {
                ForEach(store.cart, id: \.product) { line in
                    Card {
                        HStack {
                            Text("\(line.qty) × \(products[line.product].name)").font(.system(size: 17))
                            Spacer()
                            Text(money(products[line.product].cents * line.qty)).font(.system(size: 17))
                        }
                    }
                    .padding(.top, 10)
                }
                HStack {
                    Text("Total").font(.system(size: 20))
                    Spacer()
                    Text("Total \(money(store.total))").font(.system(size: 20)).foregroundColor(.accent)
                }
                .padding(.top, 20)
                Primary(title: "Checkout") { store.path.append(.checkout) }.padding(.top, 24)
            }
        }
    }
}

struct CheckoutView: View {
    @EnvironmentObject var store: Store
    @State private var name = ""
    @State private var street = ""
    @State private var city = ""
    @State private var zip = ""
    @State private var status = ""
    @State private var busy = false
    var body: some View {
        Screen {
            Title(text: "Checkout").padding(.top, 12)
            Text("Total \(money(store.total))").font(.system(size: 17)).foregroundColor(.muted)
            Field(title: "Full name", text: $name).padding(.top, 24)
            Field(title: "Street address", text: $street).padding(.top, 12)
            Field(title: "City", text: $city).padding(.top, 12)
            Field(title: "Postal code", text: $zip).padding(.top, 12)
            if !status.isEmpty {
                Text(status).font(.system(size: 15)).foregroundColor(.accent).padding(.top, 12)
            }
            Primary(title: "Place order", disabled: busy) {
                for (v, label) in [(name, "Full name"), (street, "Street address"), (city, "City"), (zip, "Postal code")]
                where v.trimmingCharacters(in: .whitespaces).isEmpty {
                    status = "Fill in \(label)"
                    return
                }
                busy = true
                status = "Placing order…"
                let who = name.trimmingCharacters(in: .whitespaces)
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.9) {
                    let total = store.total, items = store.count
                    // Deterministic per order contents; the same formula as the Android app.
                    let order = 10000 + abs((total &* 31 &+ items &* 7 &+ javaHash(who)) % 90000)
                    store.cart.removeAll()
                    store.path = [.confirmation(order: order, items: items, total: total, name: who)]
                }
            }
            .padding(.top, 12)
        }
    }
}

/// Java's String.hashCode, so both apps print the same order number.
func javaHash(_ s: String) -> Int {
    var h: Int32 = 0
    for u in s.utf16 { h = h &* 31 &+ Int32(u) }
    return Int(h)
}

struct ConfirmationView: View {
    let order: Int, items: Int, total: Int, name: String
    var body: some View {
        Screen {
            Title(text: "Order confirmed").padding(.top, 12)
            Text("Order #\(order)").font(.system(size: 22)).foregroundColor(.accent).padding(.top, 16)
            Text("\(items) items · \(money(total))").font(.system(size: 18)).foregroundColor(.ink).padding(.top, 12)
            Text("Thank you, \(name). We'll e-mail you when it ships.").font(.system(size: 17)).foregroundColor(.muted).padding(.top, 12)
        }
    }
}
