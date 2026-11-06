### android benchmark (2026-11-06)

Host: Apple M2 Pro, macOS 26.6, arm64. Device: `android:agemu-pixel`. Median of n runs (p95 and n in parentheses).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `adb exec-out screencap -p > f.png` | 152.9 ms (p95 1013.4, n=20) | 8.4 ms (p95 28.7, n=20) | 4.4 ms (p95 5.1, n=20) | **18.3×** | **35.0×** |
| screenshot (small-phone profile) | `adb exec-out screencap -p > f.png` | 111.1 ms (p95 240.8, n=20) | 7.5 ms (p95 21.0, n=20) | 3.7 ms (p95 4.3, n=20) | **14.9×** | **30.1×** |
| screenshot (after a screen change) | `adb exec-out screencap -p > f.png` | 177.4 ms (p95 488.4, n=20) | 22.1 ms (p95 92.0, n=20) | 13.3 ms (p95 21.9, n=20) | **8.0×** | **13.4×** |
| ui tree | `adb shell uiautomator dump + adb exec-out cat` | 1994.7 ms (p95 2751.6, n=20) | 5.9 ms (p95 7.9, n=20) | 1.9 ms (p95 3.2, n=20) | **336.8×** | **1051.0×** |
| agent step: find + tap + wait for next screen + screenshot | `uiautomator dump + input tap + uiautomator dump until next screen + screencap` | 5390.7 ms (p95 5633.4, n=10) | 1495.6 ms (p95 1668.5, n=10) | 1506.0 ms (p95 1694.9, n=10) | **3.6×** | **3.6×** |
| tap | `adb shell input tap 800 1100` | 52.7 ms (p95 123.7, n=20) | 4.7 ms (p95 12.5, n=20) | 0.3 ms (p95 0.4, n=20) | **11.1×** | **172.2×** |
| swipe (300 ms gesture) | `adb shell input swipe 540 1800 540 1000 300` | 383.9 ms (p95 574.6, n=20) | 309.4 ms (p95 316.3, n=20) | 302.4 ms (p95 314.6, n=20) | **1.2×** | **1.3×** |
| swipe (50 ms gesture) | `adb shell input swipe 540 1800 540 1000 50` | 129.2 ms (p95 528.3, n=20) | 58.8 ms (p95 62.2, n=20) | 51.9 ms (p95 55.7, n=20) | **2.2×** | **2.5×** |
| type text (plain field) | `adb shell input text hello` | 72.0 ms (p95 166.3, n=20) | 50.5 ms (p95 77.5, n=20) | 41.6 ms (p95 61.9, n=20) | **1.4×** | **1.7×** |
| type text --keys (plain field) | `adb shell input text hello` | 62.6 ms (p95 85.2, n=20) | 56.2 ms (p95 102.5, n=20) | 32.9 ms (p95 125.1, n=20) | **1.1×** | **1.9×** |
| type text (Settings search) | `adb shell input text hello` | 68.8 ms (p95 302.6, n=20) | 28.8 ms (p95 49.6, n=20) | 21.8 ms (p95 49.4, n=20) | **2.4×** | **3.2×** |
| key home | `adb shell input keyevent KEYCODE_HOME` | 48.9 ms (p95 137.9, n=20) | 9.5 ms (p95 23.7, n=20) | 1.2 ms (p95 10.1, n=20) | **5.1×** | **41.7×** |
| launch app (cold start) | `adb shell am start -W -n com.android.settings/.Settings` | 601.2 ms (p95 770.0, n=10) | 684.4 ms (p95 734.8, n=10) | 601.5 ms (p95 831.1, n=10) | **0.9×** | **1.0×** |
| launch app (warm start) | `adb shell am start -W -n com.android.settings/.Settings` | 86.8 ms (p95 383.0, n=10) | 135.4 ms (p95 339.4, n=10) | 78.8 ms (p95 60307.4, n=10) | **0.6×** | **1.1×** |
| profile switch | `adb shell wm size WxH && adb shell wm density D` | 166.1 ms (p95 331.3, n=10) | 836.9 ms (p95 904.8, n=10) | 829.7 ms (p95 1115.4, n=10) | **0.2×** | **0.2×** |
| boot to agent-ready (from standby) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 12482.4 ms (p95 13349.4, n=3) | 24.2 ms (p95 24.8, n=3) | 9.6 ms (p95 30.6, n=3) | **516.1×** | **1300.4×** |
| boot to agent-ready (powered off) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 12506.2 ms (p95 12692.3, n=3) | 5517.0 ms (p95 5927.6, n=3) | 5551.6 ms (p95 5667.8, n=3) | **2.3×** | **2.3×** |
| reset to clean state | `adb emu kill + emulator cold boot until sys.boot_completed=1` | 14126.8 ms (p95 14158.2, n=3) | 2222.2 ms (p95 2325.0, n=3) | 2141.3 ms (p95 2170.7, n=3) | **6.4×** | **6.6×** |
| reset vs emulator snapshot load | `adb emu avd snapshot load agemu-baseline until sys.boot_completed=1` | 2363.8 ms (p95 2421.1, n=3) | 2140.7 ms (p95 2160.2, n=3) | 2198.0 ms (p95 2230.0, n=3) | **1.1×** | **1.1×** |

- swipe (300 ms gesture): both tools must spend 300 ms on the gesture itself. Overhead on top of it: CLI 8.9×, daemon 34.4× faster.
- swipe (50 ms gesture): both tools must spend 50 ms on the gesture itself. Overhead on top of it: CLI 9.0×, daemon 41.7× faster.
