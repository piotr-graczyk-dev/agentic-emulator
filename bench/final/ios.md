### ios benchmark (2026-11-06)

Host: Apple M2 Pro, macOS 26.6, arm64. Device: `ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD`. Median of n runs (p95 and n in parentheses).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `xcrun simctl io <udid> screenshot (png)` | 145.7 ms (p95 457.8, n=20) | 8.3 ms (p95 9.9, n=20) | 4.5 ms (p95 5.0, n=20) | **17.6×** | **32.7×** |
| screenshot-jpeg | `xcrun simctl io <udid> screenshot --type=jpeg` | 118.0 ms (p95 139.3, n=20) | 9.9 ms (p95 15.6, n=20) | 5.5 ms (p95 6.4, n=20) | **11.9×** | **21.5×** |
| ui-tree | `maestro hierarchy (JVM + XCUITest runner per call)` | 7103.7 ms (p95 12583.6, n=3) | 30.4 ms (p95 55.2, n=20) | 23.0 ms (p95 27.2, n=20) | **233.4×** | **308.9×** |
| find-element x10 | `one maestro flow: 10x assertVisible` | 9019.9 ms (p95 9504.6, n=3) | 297.4 ms (p95 328.6, n=3) | 225.9 ms (p95 229.8, n=3) | **30.3×** | **39.9×** |
| tap | `maestro test (1x tapOn point)` | 8385.3 ms (p95 8551.5, n=3) | 4.5 ms (p95 20.5, n=20) | 0.3 ms (p95 1.7, n=20) | **1883.9×** | **24165.2×** |
| tap x10 | `one maestro flow: 10x tapOn point` | 16318.6 ms (p95 16391.3, n=3) | 42.9 ms (p95 65.8, n=3) | 14.6 ms (p95 16.9, n=3) | **380.4×** | **1117.6×** |
| swipe (300 ms gesture) | `maestro test (1x swipe, 300 ms)` | 9648.1 ms (p95 9706.5, n=3) | 442.4 ms (p95 462.4, n=10) | 418.2 ms (p95 453.8, n=10) | **21.8×** | **23.1×** |
| type text | `maestro test (inputText "hello")` | 8448.8 ms (p95 9176.7, n=3) | 5.3 ms (p95 15.1, n=20) | 2.5 ms (p95 4.8, n=20) | **1588.1×** | **3322.4×** |
| launch app | `xcrun simctl launch <udid> com.apple.Preferences` | 321.4 ms (p95 323.9, n=10) | 342.0 ms (p95 355.2, n=10) | 336.4 ms (p95 340.4, n=10) | **0.9×** | **1.0×** |
| launch app + first screen | `` | - | 1922.9 ms (p95 2173.0, n=5) | - | - | - |
| boot (device kept booted by agemu) | `xcrun simctl bootstatus <udid> -b` | 104.5 ms (p95 200.3, n=10) | 4.1 ms (p95 6.6, n=10) | 0.3 ms (p95 0.4, n=10) | **25.6×** | **331.8×** |
| boot (cold from shutdown) | `xcrun simctl boot + simctl bootstatus -b` | 5422.6 ms (p95 5422.6, n=2) | 7538.1 ms (p95 7538.1, n=2) | - | **0.7×** | - |
| boot (cold) + first UI tree | `` | - | 12485.8 ms (p95 12485.8, n=2) | - | - | - |
| profile switch to iphone-se-3 (cold) | `simctl shutdown <this> + simctl boot <se> + simctl bootstatus <se> -b` | 8595.4 ms (p95 8595.4, n=2) | 13039.4 ms (p95 13039.4, n=2) | - | **0.7×** | - |
| reset to clean state | `simctl shutdown + erase + boot + bootstatus -b` | 35687.2 ms (p95 35687.2, n=2) | 2075.3 ms (p95 2075.3, n=2) | - | **17.2×** | - |
