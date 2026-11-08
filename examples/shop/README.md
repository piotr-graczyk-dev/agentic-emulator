# Agemu Shop

A small store app for agent benchmarks, for Android and iOS: sign in, a catalog of 36 products
that scrolls past the screen, a search field with a 300 ms debounce, a product page with a
quantity stepper, a cart, a checkout form and an order confirmation. Sign-in and payment wait 700
and 900 ms, as a network request would. All state is in memory: a relaunch starts signed out with
an empty cart. Both apps show the same screens, texts and order number.

No Gradle and no Xcode project:

```sh
examples/shop/build.sh          # Android, plain Java -> examples/shop/build/agemu-shop.apk
examples/shop/ios/build.sh      # iOS, SwiftUI with swiftc -> examples/shop/build/AgemuShop.app
agemu install examples/shop/build/agemu-shop.apk     # or the .app on an iOS device
agemu launch dev.agemu.shop --wait-text "Sign in"
```

Demo account: `test@agemu.dev` / `hunter2`. Every control is labelled with visible text (`+`,
`Quantity: 2`, `2 products` after a search), and the iOS text fields carry an accessibility
label, so every tool finds the same elements by text.

[examples/agent-race](../agent-race) drives it through one task with `adb`, agent-device, Argent
and agemu, and records them.
