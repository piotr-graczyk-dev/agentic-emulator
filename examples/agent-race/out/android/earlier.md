# Earlier Android series, the night of 2026-11-14 to 15

The series of the night that are not the result (see the [README](../../README.md#earlier-series-the-same-night)).
agemu was v0.2.1 without the `type` fix in all of them. Agent-device 0.21.37, Argent 0.33.0,
mobile-mcp 1.0.17. One more series, between the third and the fourth here, was cut short after
8 runs (all completed) when the standard emulator was stopped by mistake.

## Series 1 (23:59-00:27, load 3-13; the mobile-mcp arm did not read the screen again after "No UI hierarchy was returned")

| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |
|---|---|---|---|---|---|
| classic-1 | yes | 77.12 s | 64 | 1916 ms | 4.59 6.53 6.23 |
| classic-2 | yes | 74.56 s | 64 | 1913 ms | 5.56 7.69 6.99 |
| classic-3 | yes | 74.87 s | 64 | 1916 ms | 3.12 5.84 6.38 |
| classic-4 | yes | 73.62 s | 64 | 1918 ms | 2.62 4.16 5.49 |
| classic-5 | yes | 75.28 s | 64 | 1913 ms | 4.03 3.88 5.00 |
| mobile-mcp-1 | no: step 4 (Sign in): mobile_batch_commands: No UI hierarchy was returned for this device - its screen is most likely off or locked. Wake it up (for example with mo | 5.97 s | 6 | 1113 ms | 5.90 6.62 6.28 |
| mobile-mcp-2 | no: step 17 (Place the order): mobile_list_elements_on_screen: No UI hierarchy was returned for this device - its screen is most likely off or locked. Wake it up (f | 31.54 s | 30 | 1201 ms | 5.93 6.07 6.11 |
| mobile-mcp-3 | yes | 30.71 s | 33 | 752 ms | 3.48 5.35 6.15 |
| mobile-mcp-4 | yes | 27.85 s | 32 | 748 ms | 2.91 3.95 5.31 |
| mobile-mcp-5 | yes | 27.75 s | 32 | 780 ms | 4.50 3.98 4.95 |
| agent-device-1 | yes | 24.64 s | 33 | 782 ms | 6.22 6.54 6.26 |
| agent-device-2 | yes | 25.91 s | 33 | 815 ms | 9.12 6.92 6.42 |
| agent-device-3 | yes | 23.62 s | 33 | 785 ms | 5.20 7.22 6.86 |
| agent-device-4 | yes | 23.75 s | 33 | 781 ms | 2.75 3.73 5.14 |
| agent-device-5 | yes | 23.71 s | 33 | 775 ms | 4.31 3.97 4.89 |
| argent-1 | yes | 31.06 s | 66 | 337 ms | 5.43 6.34 6.21 |
| argent-2 | yes | 30.35 s | 66 | 339 ms | 13.26 9.11 7.31 |
| argent-3 | yes | 29.43 s | 66 | 335 ms | 6.83 7.22 6.88 |
| argent-4 | yes | 30.19 s | 66 | 326 ms | 3.42 5.01 5.97 |
| argent-5 | yes | 30.12 s | 66 | 320 ms | 8.81 5.15 5.25 |
| agemu-1 | yes | 10.29 s | 34 | 231 ms | 4.57 6.02 6.10 |
| agemu-2 | yes | 10.21 s | 34 | 189 ms | 9.92 8.68 7.22 |
| agemu-3 | yes | 10.12 s | 34 | 188 ms | 5.37 6.83 6.75 |
| agemu-4 | yes | 10.42 s | 34 | 218 ms | 3.15 4.76 5.84 |
| agemu-5 | yes | 10.05 s | 34 | 133 ms | 2.85 3.64 5.06 |

## Series 2 (00:33-01:00, load 3-6; the mobile-mcp arm turned back after one swipe that moved nothing)

| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |
|---|---|---|---|---|---|
| classic-1 | yes | 74.91 s | 64 | 1915 ms | 3.87 4.73 4.94 |
| classic-2 | yes | 73.65 s | 64 | 1915 ms | 3.60 4.15 4.54 |
| classic-3 | yes | 76.99 s | 66 | 1921 ms | 4.67 4.49 4.59 |
| classic-4 | yes | 73.73 s | 64 | 1917 ms | 4.11 4.31 4.48 |
| classic-5 | no: step 4 (Sign in): timed out waiting for {Text:Search products Contains: Gone:} | 35.30 s | 20 | 2061 ms | 5.49 4.88 4.67 |
| mobile-mcp-1 | yes | 27.35 s | 32 | 764 ms | 3.81 4.56 4.87 |
| mobile-mcp-2 | yes | 27.47 s | 32 | 795 ms | 5.76 4.60 4.78 |
| mobile-mcp-3 | yes | 28.14 s | 32 | 767 ms | 4.88 4.55 4.60 |
| mobile-mcp-4 | yes | 27.79 s | 32 | 755 ms | 4.32 4.29 4.46 |
| mobile-mcp-5 | no: step 5 (Scroll to Bath Towel): "Bath Towel" not found after scrolling | 41.98 s | 28 | 1784 ms | 4.90 4.69 4.61 |
| agent-device-1 | yes | 24.44 s | 33 | 778 ms | 3.85 4.43 4.80 |
| agent-device-2 | yes | 23.82 s | 33 | 769 ms | 4.99 4.56 4.75 |
| agent-device-3 | yes | 23.87 s | 33 | 772 ms | 4.38 4.22 4.54 |
| agent-device-4 | yes | 26.65 s | 33 | 780 ms | 5.36 4.67 4.59 |
| agent-device-5 | yes | 23.59 s | 33 | 781 ms | 3.66 4.36 4.49 |
| argent-1 | yes | 29.83 s | 66 | 322 ms | 4.00 4.45 4.78 |
| argent-2 | yes | 30.37 s | 66 | 328 ms | 4.59 4.54 4.74 |
| argent-3 | yes | 30.08 s | 66 | 329 ms | 5.69 4.56 4.64 |
| argent-4 | yes | 29.71 s | 66 | 324 ms | 5.22 4.60 4.61 |
| argent-5 | yes | 29.31 s | 66 | 319 ms | 2.92 4.05 4.36 |
| agemu-1 | yes | 10.27 s | 34 | 251 ms | 3.76 4.32 4.72 |
| agemu-2 | yes | 10.41 s | 34 | 215 ms | 3.95 4.39 4.67 |
| agemu-3 | yes | 9.91 s | 34 | 124 ms | 4.70 4.42 4.59 |
| agemu-4 | yes | 10.46 s | 34 | 222 ms | 4.33 4.45 4.56 |
| agemu-5 | yes | 10.29 s | 34 | 164 ms | 5.55 4.78 4.63 |

## Series 3 (01:01-01:29, load 3-12; 4.3 GB of swap in use at its end)

| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |
|---|---|---|---|---|---|
| classic-1 | yes | 75.36 s | 64 | 1919 ms | 3.17 3.91 4.26 |
| classic-2 | yes | 88.70 s | 66 | 1922 ms | 10.33 7.26 5.57 |
| classic-3 | yes | 85.50 s | 64 | 1918 ms | 9.19 8.10 6.40 |
| classic-4 | yes | 86.01 s | 66 | 1918 ms | 11.65 9.65 7.56 |
| classic-5 | no: step 4 (Sign in): timed out waiting for {Text:Search products Contains: Gone:} | 42.19 s | 20 | 2149 ms | 7.51 8.52 7.55 |
| mobile-mcp-1 | yes | 28.43 s | 32 | 755 ms | 4.32 4.06 4.28 |
| mobile-mcp-2 | yes | 30.45 s | 32 | 755 ms | 5.02 4.28 4.30 |
| mobile-mcp-3 | yes | 31.74 s | 32 | 819 ms | 9.89 8.56 6.71 |
| mobile-mcp-4 | yes | 30.62 s | 32 | 852 ms | 9.74 9.47 7.65 |
| mobile-mcp-5 | yes | 39.29 s | 35 | 951 ms | 9.35 9.05 7.85 |
| agent-device-1 | yes | 23.85 s | 33 | 781 ms | 3.74 3.93 4.22 |
| agent-device-2 | yes | 23.54 s | 33 | 783 ms | 5.43 4.47 4.36 |
| agent-device-3 | yes | 26.82 s | 33 | 781 ms | 10.91 8.08 6.00 |
| agent-device-4 | yes | 24.81 s | 33 | 782 ms | 7.40 8.90 7.56 |
| agent-device-5 | yes | 30.98 s | 33 | 782 ms | 10.81 9.54 8.11 |
| argent-1 | yes | 29.49 s | 66 | 334 ms | 3.96 3.94 4.20 |
| argent-2 | no: step 5 (Scroll to Bath Towel): "Bath Towel" not found after scrolling | 25.27 s | 28 | 716 ms | 9.47 5.76 4.84 |
| argent-3 | yes | 33.09 s | 66 | 336 ms | 8.69 7.80 6.02 |
| argent-4 | yes | 32.34 s | 66 | 329 ms | 10.41 9.00 7.01 |
| argent-5 | yes | 30.63 s | 66 | 333 ms | 7.66 8.90 7.97 |
| agemu-1 | yes | 10.27 s | 34 | 216 ms | 4.47 4.06 4.23 |
| agemu-2 | yes | 10.38 s | 34 | 228 ms | 9.34 6.10 5.00 |
| agemu-3 | no: step 4 (Sign in): exit status 1: error: tap done, but: timeout after 10s waiting for {"text":"Search products"} to appear | 10.97 s | 6 | 142 ms | 7.90 7.73 6.07 |
| agemu-4 | yes | 10.05 s | 34 | 219 ms | 10.54 9.17 7.14 |
| agemu-5 | yes | 10.37 s | 34 | 242 ms | 7.55 8.71 7.54 |

## Series 4 (02:15-02:51, load 4-15; swap in use grew from 5.9 to 9.5 GB)

| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |
|---|---|---|---|---|---|
| classic-1 | yes | 83.77 s | 64 | 1936 ms | 5.62 5.55 6.33 |
| classic-2 | yes | 80.69 s | 64 | 1927 ms | 7.20 6.88 6.93 |
| classic-3 | yes | 111.09 s | 66 | 1949 ms | 9.36 10.86 8.83 |
| classic-4 | yes | 88.09 s | 64 | 1925 ms | 7.00 8.25 8.16 |
| classic-5 | yes | 89.85 s | 66 | 1925 ms | 6.89 7.08 7.61 |
| mobile-mcp-1 | yes | 40.51 s | 31 | 1120 ms | 7.40 5.98 6.39 |
| mobile-mcp-2 | yes | 29.72 s | 32 | 873 ms | 4.40 6.81 6.95 |
| mobile-mcp-3 | yes | 35.60 s | 30 | 909 ms | 6.27 9.34 8.46 |
| mobile-mcp-4 | yes | 34.23 s | 35 | 950 ms | 6.59 7.88 8.03 |
| mobile-mcp-5 | yes | 47.73 s | 37 | 1124 ms | 9.91 7.82 7.81 |
| agent-device-1 | no: step 1 (Open the app): exit status 1: Error (COMMAND_FAILED): Could not read process state for dev.agemu.shop. May have reached the device. Next: Run snapshot t | 2.16 s | 1 | 2163 ms | 15.26 8.90 7.45 |
| agent-device-2 | yes | 25.67 s | 33 | 789 ms | 7.50 7.08 7.03 |
| agent-device-3 | yes | 26.90 s | 33 | 782 ms | 8.92 7.49 7.16 |
| agent-device-4 | yes | 27.32 s | 33 | 787 ms | 5.29 7.16 7.74 |
| agent-device-5 | yes | 58.85 s | 33 | 788 ms | 14.73 9.70 8.51 |
| argent-1 | no: step 4 (Sign in): exit status 1: await-ui-element condition not met: no element matched the selector before timeout | 32.56 s | 11 | 1333 ms | 7.13 8.12 7.38 |
| argent-2 | yes | 33.83 s | 66 | 367 ms | 7.38 7.09 7.03 |
| argent-3 | yes | 36.99 s | 66 | 352 ms | 9.29 7.79 7.29 |
| argent-4 | yes | 32.29 s | 66 | 367 ms | 5.90 8.60 8.24 |
| argent-5 | yes | 46.53 s | 66 | 427 ms | 9.86 9.46 8.53 |
| agemu-1 | yes | 10.03 s | 34 | 184 ms | 5.40 7.56 7.20 |
| agemu-2 | yes | 10.75 s | 34 | 218 ms | 7.18 7.04 7.01 |
| agemu-3 | yes | 10.71 s | 34 | 173 ms | 8.60 7.78 7.30 |
| agemu-4 | yes | 10.77 s | 34 | 172 ms | 6.67 8.46 8.20 |
| agemu-5 | yes | 10.02 s | 34 | 114 ms | 5.31 6.93 7.62 |

## Series 5 (04:10-04:49, load 3-8; swap in use 3.8-5.1 GB; the mobile-mcp arm still turned back after one lost swipe)

| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |
|---|---|---|---|---|---|
| classic-1 | yes | 81.94 s | 64 | 1932 ms | 5.11 6.17 6.87 |
| classic-2 | yes | 78.11 s | 64 | 1922 ms | 6.29 5.06 5.77 |
| classic-3 | yes | 74.27 s | 64 | 1915 ms | 2.88 4.23 5.22 |
| classic-4 | yes | 74.45 s | 64 | 1917 ms | 4.43 4.79 5.20 |
| classic-5 | yes | 74.56 s | 64 | 1919 ms | 3.34 4.00 4.74 |
| mobile-mcp-1 | yes | 29.75 s | 31 | 955 ms | 4.84 5.83 6.68 |
| mobile-mcp-2 | yes | 29.27 s | 32 | 757 ms | 3.94 5.30 6.16 |
| mobile-mcp-3 | yes | 43.16 s | 39 | 1178 ms | 5.59 4.65 5.28 |
| mobile-mcp-4 | yes | 28.07 s | 32 | 766 ms | 3.45 4.42 5.03 |
| mobile-mcp-5 | no: step 5 (Scroll to Bath Towel): "Bath Towel" not found after scrolling | 50.39 s | 28 | 1841 ms | 5.74 4.55 4.87 |
| agent-device-1 | yes | 56.93 s | 33 | 800 ms | 7.13 6.31 6.77 |
| agent-device-2 | yes | 26.61 s | 33 | 779 ms | 3.67 4.97 5.97 |
| agent-device-3 | yes | 25.21 s | 33 | 797 ms | 4.70 4.88 5.63 |
| agent-device-4 | yes | 23.73 s | 33 | 789 ms | 3.30 4.25 4.93 |
| agent-device-5 | yes | 24.18 s | 33 | 793 ms | 5.04 4.57 4.85 |
| argent-1 | yes | 34.11 s | 66 | 327 ms | 6.00 6.02 6.57 |
| argent-2 | yes | 29.84 s | 66 | 334 ms | 3.58 4.70 5.81 |
| argent-3 | yes | 30.00 s | 66 | 330 ms | 4.42 4.74 5.52 |
| argent-4 | yes | 30.17 s | 66 | 323 ms | 8.27 5.40 5.50 |
| argent-5 | yes | 29.15 s | 66 | 325 ms | 5.03 4.62 4.84 |
| agemu-1 | yes | 10.92 s | 34 | 235 ms | 4.85 5.74 6.38 |
| agemu-2 | yes | 10.21 s | 34 | 197 ms | 3.91 4.67 5.75 |
| agemu-3 | yes | 10.28 s | 34 | 190 ms | 4.80 4.78 5.51 |
| agemu-4 | no: step 17 (Place the order): exit status 1: error: tap done, but: timeout after 10s waiting for {"contains":"Order #"} to appear | 18.65 s | 32 | 229 ms | 4.30 4.83 5.27 |
| agemu-5 | yes | 10.44 s | 34 | 249 ms | 3.18 4.11 4.86 |
