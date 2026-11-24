---
name: check-on-device
description: Build a mobile app, run it on an iOS Simulator or Android emulator through agemu, and check a change on screen. Use after changing app code (React Native, Expo, native iOS or Android) when the result should be seen working on a device, when reproducing a bug in the running app, or when asked to test a flow, read the app's logs or look into a crash.
---

# Check a change on a device with agemu

The loop is: build, install, launch, act, check, read the logs, reset. agemu's tools answer in
milliseconds, so the time goes to your turns. Use as few as the check needs.

## 1. Pick a device

- Call `devices`. Use a booted one. If none is booted, `boot` an `agemu-*` device; if none
  exists, `create` one (`platform` ios or android), then `boot` it.
- A new Android device prepares once, for 7-8 minutes: `boot` returns state `preparing`. Poll
  `devices` every 30-60 s and do other work meanwhile.
- When more than one device is booted, pass `device` in every call.
- agemu only touches devices named `agemu-*`. The user's other simulators and emulators stay as
  they are.

## 2. Build and install

Prefer the project's own build command. The usual ones:

| Project | Build | Artifact to install |
|---|---|---|
| iOS (native; React Native's is in `ios/`) | `xcodebuild -workspace <App>.xcworkspace -scheme <App> -configuration Debug -sdk iphonesimulator -derivedDataPath build` | `build/Build/Products/Debug-iphonesimulator/<App>.app` |
| Android (native; React Native's is in `android/`) | `./gradlew assembleDebug` | `app/build/outputs/apk/debug/app-debug.apk` |
| Expo | `npx expo prebuild` once, then as React Native | the same paths |

- `install` takes the artifact's absolute path. Then `launch` the app id with `waitFor` set to
  something on the first screen, and `returnUi: true` to get that screen back in the same call.
- Expo development build on iOS: start Metro (`npx expo start --dev-client`), then `launch` with
  `relaunch: true` and `args: ["--initialUrl", "http://127.0.0.1:8081"]`.
- On the Android emulator the Mac is `10.0.2.2`, so a development build reaches Metro at
  `http://10.0.2.2:8081`.
- A JavaScript-only change with Metro running needs no rebuild: the app reloads by itself.
  Relaunch only when the check needs a fresh state.

## 3. Act and check, one call per step

- Tap by `text`, `contains` or `id`, not by coordinates. A tap by selector waits until the
  element can take the touch.
- Give the action `returnUi: true`. The call then acts and returns the screen it ends on, read
  once it has stopped changing. Do not call `ui_tree` after it.
- When the next screen comes after a request (a sign-in, a payment), also give `waitFor` with
  something that screen shows.
- Fill a field in one call: `type_text` with `into: {"text": "<the field's label>"}`. The text
  is added to what the field holds; `clear: true` replaces it. Do not delete character by
  character.
- Scroll with `swipe` and `returnUi: true`, until the element is in the outline.
- Check a result with `wait_for` or `find`, on text or id. Read text from the outline, not from
  a screenshot.
- Take a `screenshot` when the look is the question: layout, colours, images, clipping. Its
  coordinates at `scale: 1` are tap coordinates.

## 4. When something fails

- `logs` with the app id, `since: "launch"` and `level: "warn"` shows what the app logged in this
  run. `crashes` with `since: "launch"` gives the crash, ANR or system kill with its top frames.
- When the app you launched dies, the next result starts with a NOTE line. Read `crashes` before
  trying again.
- React Native's `console.log` output is in Metro's terminal.
- A wait that timed out names what it waited for. Read `ui_tree` once to see the screen as it
  is, then decide.

## 5. Set up and clean up

- Test setup without dialogs: `permission` (grant, revoke, reset), `location`, `appearance`
  (light or dark), `clipboard`, `open_url` for deep links, `push` on iOS.
- `clear_app_data` puts one app back to a fresh install. `reset` puts the whole device back to
  its clean baseline in about two seconds, and removes the apps installed since.
- When the check is done and the session goes on, `park` the device: it stops using CPU, and
  the next call resumes it.

## 6. Report

Say what you checked and what the screen showed, quoting the text you read. Give the
screenshot's path if you took one, and the log lines or crash frames that explain a failure.
If you could not check something on the device, say so.
