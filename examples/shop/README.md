# Agemu Shop

A small Android store app for agent benchmarks: sign in, a catalog of 24 products that scrolls
past the screen, a search field with a 300 ms debounce, a product page with a quantity stepper, a
cart, a checkout form and an order confirmation. Sign-in and payment wait 700 and 900 ms, like a
network request would. All state is in memory: a force-stop starts it signed out with an empty
cart.

Plain Java, no resources and no Gradle:

```sh
examples/shop/build.sh                 # -> examples/shop/build/agemu-shop.apk
agemu install examples/shop/build/agemu-shop.apk
agemu launch dev.agemu.shop --wait-text "Sign in"
```

Demo account: `test@agemu.dev` / `hunter2`. Every control has visible text or a content
description (`Increase quantity`, `Quantity 2`), so an agent can find it by text in the UI tree.

[examples/agent-race](../agent-race) drives it through one task with the standard tools and with
agemu, and records both.
