package agemu;

import android.accessibilityservice.AccessibilityServiceInfo;
import android.app.UiAutomation;
import android.content.ComponentName;
import android.content.Intent;
import android.content.pm.ResolveInfo;
import android.graphics.Point;
import android.graphics.Rect;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.os.Bundle;
import android.os.HandlerThread;
import android.os.IBinder;
import android.os.Looper;
import android.os.Process;
import android.os.SystemClock;
import android.view.InputDevice;
import android.view.KeyCharacterMap;
import android.view.KeyEvent;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityWindowInfo;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * agemu on-device agent. Started by the host as the shell user:
 *
 *   CLASSPATH=/data/local/tmp/agemu-agent.jar app_process /system/bin agemu.Agent [socket-name]
 *
 * It keeps one UiAutomation connection for its whole life and serves newline-delimited
 * JSON requests on an abstract unix socket (default "agemu-agent"). The host reaches the
 * socket through the adb server ("localabstract:agemu-agent"), so no port forward is needed.
 *
 * Request:  {"id":1,"cmd":"tree"}
 * Response: {"id":1,"ok":true,"result":{...}}  or  {"id":1,"ok":false,"error":"..."}
 *
 * With a second argument (a TCP port) the agent also listens on that port of the guest. The
 * host reaches it through an emulator console port redirection (`redir add`), which, unlike
 * adb, works the moment a restored snapshot resumes. A TCP client must first send the line
 * {"auth":"<token>"} with the token from the AGEMU_TOKEN environment variable.
 */
public final class Agent {
    static final int USER_CURRENT = -2;

    // Node flag bits (kept in sync with internal/android/uitree.go).
    static final int F_ENABLED = 1, F_FOCUSED = 2, F_SELECTED = 4, F_CHECKABLE = 8, F_CHECKED = 16,
            F_CLICKABLE = 32, F_LONG_CLICKABLE = 64, F_SCROLLABLE = 128, F_PASSWORD = 256,
            F_EDITABLE = 512, F_FOCUSABLE = 1024, F_HINT = 2048;

    static String version = "unknown";
    static UiAutomation ua;
    static final Object uaLock = new Object();

    public static void main(String[] args) throws Exception {
        String sockName = args.length > 0 ? args[0] : "agemu-agent";
        version = jarHash();
        Looper.prepareMainLooper();
        ua = connectUiAutomation();
        final LocalServerSocket server = new LocalServerSocket(sockName);
        System.out.println("agemu-agent " + version + " listening on @" + sockName + " pid " + Process.myPid());
        Thread t = new Thread(new Runnable() {
            public void run() {
                while (true) {
                    try {
                        final LocalSocket s = server.accept();
                        Thread h = new Thread(new Runnable() {
                            public void run() {
                                try {
                                    serve(s.getInputStream(), s.getOutputStream(), null);
                                } catch (Throwable ignored) {
                                } finally {
                                    try { s.close(); } catch (Throwable ignored) {}
                                }
                            }
                        }, "agemu-conn");
                        h.setDaemon(true);
                        h.start();
                    } catch (Throwable e) {
                        System.err.println("accept: " + e);
                        SystemClock.sleep(100);
                    }
                }
            }
        }, "agemu-accept");
        t.start();
        final int tcpPort = args.length > 1 ? Integer.parseInt(args[1]) : 0;
        final String token = System.getenv("AGEMU_TOKEN");
        if (tcpPort > 0 && token != null && token.length() >= 16) {
            Thread tt = new Thread(new Runnable() {
                public void run() { serveTCP(tcpPort, token); }
            }, "agemu-tcp");
            tt.setDaemon(true);
            tt.start();
        }
        Looper.loop();
    }

    static volatile int tcpListening = 0;

    static void serveTCP(int port, final String token) {
        ServerSocket ss = null;
        for (int i = 0; ss == null; i++) {
            try {
                ss = new ServerSocket();
                ss.setReuseAddress(true);
                ss.bind(new InetSocketAddress(port), 16);
            } catch (Throwable e) {
                try { if (ss != null) ss.close(); } catch (Throwable ignored) {}
                ss = null;
                if (i == 50) { System.err.println("tcp: " + e); return; }
                SystemClock.sleep(100); // a previous agent may still hold the port
            }
        }
        tcpListening = port;
        System.out.println("agemu-agent listening on tcp:" + port);
        while (true) {
            try {
                final Socket s = ss.accept();
                s.setTcpNoDelay(true);
                Thread h = new Thread(new Runnable() {
                    public void run() {
                        try {
                            serve(s.getInputStream(), s.getOutputStream(), token);
                        } catch (Throwable ignored) {
                        } finally {
                            try { s.close(); } catch (Throwable ignored) {}
                        }
                    }
                }, "agemu-tcp-conn");
                h.setDaemon(true);
                h.start();
            } catch (Throwable e) {
                SystemClock.sleep(100);
            }
        }
    }

    static String jarHash() {
        try {
            String cp = System.getenv("CLASSPATH");
            if (cp == null) return "unknown";
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            try (FileInputStream in = new FileInputStream(new File(cp.split(":")[0]))) {
                byte[] buf = new byte[65536];
                int n;
                while ((n = in.read(buf)) > 0) md.update(buf, 0, n);
            }
            StringBuilder sb = new StringBuilder();
            for (byte b : md.digest()) sb.append(String.format("%02x", b));
            return sb.toString();
        } catch (Throwable e) {
            return "unknown";
        }
    }

    static UiAutomation connectUiAutomation() throws Exception {
        HandlerThread ht = new HandlerThread("agemu-uia");
        ht.start();
        Class<?> connCls = Class.forName("android.app.UiAutomationConnection");
        Object conn = connCls.getConstructor().newInstance();
        Class<?> iconn = Class.forName("android.app.IUiAutomationConnection");
        UiAutomation u = (UiAutomation) UiAutomation.class.getConstructor(Looper.class, iconn)
                .newInstance(ht.getLooper(), conn);
        UiAutomation.class.getMethod("connect", int.class)
                .invoke(u, UiAutomation.FLAG_DONT_SUPPRESS_ACCESSIBILITY_SERVICES);
        AccessibilityServiceInfo info = u.getServiceInfo();
        info.flags |= AccessibilityServiceInfo.FLAG_REPORT_VIEW_IDS
                | AccessibilityServiceInfo.FLAG_RETRIEVE_INTERACTIVE_WINDOWS
                | AccessibilityServiceInfo.FLAG_INCLUDE_NOT_IMPORTANT_VIEWS;
        u.setServiceInfo(info);
        return u;
    }

    // ---------------------------------------------------------------- server

    /** Serves one connection. With a token, the first line must be {"auth":"<token>"}. */
    static void serve(InputStream in, OutputStream out, String token) throws Exception {
        BufferedReader r = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8), 1 << 16);
        String line;
        if (token != null) {
            line = r.readLine();
            if (line == null) return;
            JSONObject a = new JSONObject(line);
            if (!MessageDigest.isEqual(token.getBytes(StandardCharsets.UTF_8), a.optString("auth").getBytes(StandardCharsets.UTF_8))) {
                out.write("{\"ok\":false,\"error\":\"bad token\"}\n".getBytes(StandardCharsets.UTF_8));
                out.flush();
                return;
            }
            out.write("{\"ok\":true}\n".getBytes(StandardCharsets.UTF_8));
            out.flush();
        }
        while ((line = r.readLine()) != null) {
            if (line.isEmpty()) continue;
            long id = 0;
            StringBuilder sb = new StringBuilder(1 << 14);
            try {
                JSONObject req = new JSONObject(line);
                id = req.optLong("id");
                sb.append("{\"id\":").append(id).append(",\"ok\":true,\"result\":");
                handle(req, sb);
                sb.append("}\n");
            } catch (Throwable e) {
                Throwable c = e;
                while (c.getCause() != null) c = c.getCause();
                sb.setLength(0);
                sb.append("{\"id\":").append(id).append(",\"ok\":false,\"error\":");
                str(sb, c.getClass().getSimpleName() + ": " + c.getMessage());
                sb.append("}\n");
            }
            out.write(sb.toString().getBytes(StandardCharsets.UTF_8));
            out.flush();
        }
    }

    static void handle(JSONObject req, StringBuilder sb) throws Exception {
        String cmd = req.getString("cmd");
        switch (cmd) {
            case "ping":
                sb.append("{\"version\":"); str(sb, version);
                sb.append(",\"pid\":").append(Process.myPid());
                sb.append(",\"uptimeMs\":").append(SystemClock.uptimeMillis());
                sb.append(",\"bootCompleted\":"); str(sb, sysprop("sys.boot_completed"));
                sb.append(",\"tcp\":").append(tcpListening);
                sb.append("}");
                return;
            case "display":
                display(sb);
                return;
            case "tree":
                synchronized (uaLock) { tree(sb); }
                return;
            case "profile":
                profile(req.getInt("w"), req.getInt("h"), req.getInt("density"), req.optLong("settleMs", 3000));
                display(sb);
                return;
            case "idle":
                sb.append(idle(req.optLong("quietMs", 100), req.optLong("timeoutMs", 2000)) ? "true" : "false");
                return;
            case "launch":
                launch(req.getString("pkg"), req.optString("activity", ""), sb);
                return;
            case "stop":
                forceStop(req.getString("pkg"));
                sb.append("true");
                return;
            case "text": {
                String mode = typeText(req.getString("text"), req.optBoolean("keys", false));
                sb.append("{\"mode\":"); str(sb, mode); sb.append('}');
                return;
            }
            case "key":
                injectKey(req.getInt("code"), req.optBoolean("async", false));
                sb.append("true");
                return;
            case "fg":
                str(sb, foreground());
                return;
            case "cleartasks": {
                // Remove recent tasks (what swiping everything away in Overview does).
                Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
                atm.getClass().getMethod("removeAllVisibleRecentTasks").invoke(atm);
                sb.append("true");
                return;
            }
            case "exit":
                sb.append("true");
                new Thread(new Runnable() {
                    public void run() { SystemClock.sleep(50); System.exit(0); }
                }).start();
                return;
            default:
                throw new IllegalArgumentException("unknown cmd " + cmd);
        }
    }

    // ---------------------------------------------------------------- display / profiles

    static Object displayInfo() throws Exception {
        Class<?> g = Class.forName("android.hardware.display.DisplayManagerGlobal");
        Object dmg = g.getMethod("getInstance").invoke(null);
        return g.getMethod("getDisplayInfo", int.class).invoke(dmg, 0);
    }

    static int intField(Object o, String name) throws Exception {
        Field f = o.getClass().getField(name);
        return f.getInt(o);
    }

    static Object windowManager() throws Exception {
        IBinder b = (IBinder) Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, "window");
        return Class.forName("android.view.IWindowManager$Stub").getMethod("asInterface", IBinder.class).invoke(null, b);
    }

    static void display(StringBuilder sb) throws Exception {
        Object di = displayInfo();
        Object wm = windowManager();
        Point init = new Point();
        wm.getClass().getMethod("getInitialDisplaySize", int.class, Point.class).invoke(wm, 0, init);
        int initDensity = (Integer) wm.getClass().getMethod("getInitialDisplayDensity", int.class).invoke(wm, 0);
        sb.append("{\"w\":").append(intField(di, "logicalWidth"));
        sb.append(",\"h\":").append(intField(di, "logicalHeight"));
        sb.append(",\"density\":").append(intField(di, "logicalDensityDpi"));
        sb.append(",\"rotation\":").append(intField(di, "rotation"));
        sb.append(",\"physW\":").append(init.x);
        sb.append(",\"physH\":").append(init.y);
        sb.append(",\"physDensity\":").append(initDensity);
        sb.append("}");
    }

    /** Applies a display size + density override (like `wm size` / `wm density`) and waits until it is live. */
    static void profile(int w, int h, int density, long settleMs) throws Exception {
        Object wm = windowManager();
        Class<?> c = wm.getClass();
        Point init = new Point();
        c.getMethod("getInitialDisplaySize", int.class, Point.class).invoke(wm, 0, init);
        int initDensity = (Integer) c.getMethod("getInitialDisplayDensity", int.class).invoke(wm, 0);
        if (w == init.x && h == init.y) {
            c.getMethod("clearForcedDisplaySize", int.class).invoke(wm, 0);
        } else {
            c.getMethod("setForcedDisplaySize", int.class, int.class, int.class).invoke(wm, 0, w, h);
        }
        if (density == initDensity) {
            c.getMethod("clearForcedDisplayDensityForUser", int.class, int.class).invoke(wm, 0, USER_CURRENT);
        } else {
            c.getMethod("setForcedDisplayDensityForUser", int.class, int.class, int.class).invoke(wm, 0, density, USER_CURRENT);
        }
        long t0 = SystemClock.uptimeMillis();
        while (SystemClock.uptimeMillis() - t0 < 5000) {
            Object di = displayInfo();
            if (intField(di, "logicalWidth") == w && intField(di, "logicalHeight") == h
                    && intField(di, "logicalDensityDpi") == density) break;
            SystemClock.sleep(5);
        }
        if (settleMs <= 0) return;
        // Apps relaunch / relayout for the new configuration. Wait until the foreground app's
        // window covers the new display and exposes content again, then for a quiet period.
        long t1 = SystemClock.uptimeMillis();
        while (SystemClock.uptimeMillis() - t1 < settleMs) {
            if (fgWindowFits(w, h)) break;
            SystemClock.sleep(10);
        }
        long left = settleMs - (SystemClock.uptimeMillis() - t1);
        idle(300, Math.max(500, left));
        // The display change also runs a window cross-fade; wait until WindowManager has no
        // animations left so the next screenshot shows the settled screen.
        waitForWindowAnimations();
    }

    static void waitForWindowAnimations() {
        try {
            Object wm = windowManager();
            wm.getClass().getMethod("syncInputTransactions", boolean.class).invoke(wm, true);
        } catch (Throwable ignored) {
            SystemClock.sleep(500);
        }
    }

    static boolean fgWindowFits(int w, int h) {
        try {
            for (AccessibilityWindowInfo win : ua.getWindows()) {
                if (win.getType() != AccessibilityWindowInfo.TYPE_APPLICATION || !(win.isActive() || win.isFocused())) continue;
                Rect b = new Rect();
                win.getBoundsInScreen(b);
                AccessibilityNodeInfo r = win.getRoot();
                if (r == null || r.getChildCount() == 0) return false;
                Rect rb = new Rect();
                r.getBoundsInScreen(rb);
                return b.width() == w && b.height() == h && rb.width() == w && rb.height() == h;
            }
        } catch (Throwable ignored) {}
        return false;
    }

    static boolean idle(long quietMs, long timeoutMs) {
        try {
            ua.waitForIdle(quietMs, timeoutMs);
            return true;
        } catch (Throwable e) {
            return false;
        }
    }

    // ---------------------------------------------------------------- UI tree

    static void tree(StringBuilder sb) throws Exception {
        Object di = displayInfo();
        int dw = intField(di, "logicalWidth"), dh = intField(di, "logicalHeight");
        Rect screen = new Rect(0, 0, dw, dh);
        List<AccessibilityWindowInfo> ws = new ArrayList<>(ua.getWindows());
        // Top-most first, so selectors find what is visually in front (dialogs, keyboard).
        Collections.sort(ws, new java.util.Comparator<AccessibilityWindowInfo>() {
            public int compare(AccessibilityWindowInfo a, AccessibilityWindowInfo b) { return Integer.compare(b.getLayer(), a.getLayer()); }
        });
        String app = "";
        sb.append("{\"w\":").append(dw).append(",\"h\":").append(dh);
        sb.append(",\"density\":").append(intField(di, "logicalDensityDpi"));
        sb.append(",\"rotation\":").append(intField(di, "rotation"));
        sb.append(",\"windows\":[");
        boolean first = true;
        for (AccessibilityWindowInfo w : ws) {
            AccessibilityNodeInfo root = w.getRoot();
            Rect wb = new Rect();
            w.getBoundsInScreen(wb);
            if (!wb.intersect(screen)) continue;
            if (!first) sb.append(',');
            first = false;
            sb.append("{\"type\":").append(w.getType());
            sb.append(",\"layer\":").append(w.getLayer());
            sb.append(",\"active\":").append(w.isActive());
            sb.append(",\"focused\":").append(w.isFocused());
            sb.append(",\"title\":"); str(sb, cs(w.getTitle()));
            sb.append(",\"b\":["); rect(sb, wb); sb.append(']');
            if (root != null) {
                String pkg = cs(root.getPackageName());
                sb.append(",\"pkg\":"); str(sb, pkg);
                if (app.isEmpty() && w.getType() == AccessibilityWindowInfo.TYPE_APPLICATION && (w.isActive() || w.isFocused())) app = pkg;
                sb.append(",\"root\":");
                node(sb, root, wb, 0);
            }
            sb.append('}');
        }
        sb.append("],\"app\":"); str(sb, app);
        sb.append('}');
    }

    static void node(StringBuilder sb, AccessibilityNodeInfo n, Rect clip, int depth) {
        Rect b = new Rect();
        n.getBoundsInScreen(b);
        if (!b.intersect(clip)) b.setEmpty();
        int f = 0;
        if (n.isEnabled()) f |= F_ENABLED;
        if (n.isFocused()) f |= F_FOCUSED;
        if (n.isSelected()) f |= F_SELECTED;
        if (n.isCheckable()) f |= F_CHECKABLE;
        if (n.isChecked()) f |= F_CHECKED;
        if (n.isClickable()) f |= F_CLICKABLE;
        if (n.isLongClickable()) f |= F_LONG_CLICKABLE;
        if (n.isScrollable()) f |= F_SCROLLABLE;
        if (n.isPassword()) f |= F_PASSWORD;
        if (n.isEditable()) f |= F_EDITABLE;
        if (n.isFocusable()) f |= F_FOCUSABLE;
        if (n.isShowingHintText()) f |= F_HINT;
        sb.append("{\"cls\":"); str(sb, cs(n.getClassName()));
        String t = cs(n.getText());
        if (!t.isEmpty()) { sb.append(",\"text\":"); str(sb, t); }
        String d = cs(n.getContentDescription());
        if (!d.isEmpty()) { sb.append(",\"desc\":"); str(sb, d); }
        String id = n.getViewIdResourceName();
        if (id != null && !id.isEmpty()) { sb.append(",\"id\":"); str(sb, id); }
        String hint = cs(n.getHintText());
        if (!hint.isEmpty()) { sb.append(",\"hint\":"); str(sb, hint); }
        String sd = cs(n.getStateDescription());
        if (!sd.isEmpty()) { sb.append(",\"state\":"); str(sb, sd); }
        sb.append(",\"b\":["); rect(sb, b); sb.append("],\"f\":").append(f);
        int cc = n.getChildCount();
        if (cc > 0 && depth < 120) {
            boolean any = false;
            for (int i = 0; i < cc; i++) {
                AccessibilityNodeInfo c;
                try { c = n.getChild(i); } catch (Throwable e) { c = null; }
                if (c == null || !c.isVisibleToUser()) continue;
                sb.append(any ? "," : ",\"c\":[");
                any = true;
                node(sb, c, clip, depth + 1);
            }
            if (any) sb.append(']');
        }
        sb.append('}');
    }

    static String foreground() {
        try {
            for (AccessibilityWindowInfo w : ua.getWindows()) {
                if (w.getType() == AccessibilityWindowInfo.TYPE_APPLICATION && (w.isActive() || w.isFocused())) {
                    AccessibilityNodeInfo r = w.getRoot();
                    if (r != null) return cs(r.getPackageName());
                }
            }
        } catch (Throwable ignored) {}
        return "";
    }

    // ---------------------------------------------------------------- apps

    static void launch(String pkg, String activity, StringBuilder sb) throws Exception {
        ComponentName cn;
        if (!activity.isEmpty()) {
            cn = new ComponentName(pkg, activity.startsWith(".") ? pkg + activity : activity);
        } else {
            Intent q = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER).setPackage(pkg);
            Object pm = Class.forName("android.app.ActivityThread").getMethod("getPackageManager").invoke(null);
            Method qm = null;
            for (Method m : pm.getClass().getMethods()) if (m.getName().equals("queryIntentActivities") && m.getParameterTypes().length == 4) qm = m;
            if (qm == null) throw new IllegalStateException("queryIntentActivities not found");
            Object flags = qm.getParameterTypes()[2] == long.class ? (Object) 0L : (Object) 0;
            Object slice = qm.invoke(pm, q, null, flags, 0);
            @SuppressWarnings("unchecked")
            List<ResolveInfo> list = (List<ResolveInfo>) slice.getClass().getMethod("getList").invoke(slice);
            if (list == null || list.isEmpty()) throw new IllegalArgumentException("no launchable activity for " + pkg);
            ResolveInfo ri = list.get(0);
            cn = new ComponentName(ri.activityInfo.packageName, ri.activityInfo.name);
        }
        String prevFg = foreground();
        Intent i = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER).setComponent(cn)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
        Method sm = null;
        for (Method m : atm.getClass().getMethods()) if (m.getName().equals("startActivityAndWait")) sm = m;
        if (sm == null) throw new IllegalStateException("startActivityAndWait not found");
        // (caller, callingPackage, callingFeatureId, intent, resolvedType, resultTo, resultWho,
        //  requestCode, flags, profilerInfo, options, userId) - the same call `am start -W` makes.
        Object wr = sm.invoke(atm, null, "com.android.shell", null, i, null, null, null, 0, 0, null, null, USER_CURRENT);
        int result = wr.getClass().getField("result").getInt(wr);
        long total = wr.getClass().getField("totalTime").getLong(wr);
        if (result < 0) throw new IllegalStateException("start failed, result " + result + " for " + cn.flattenToShortString());
        // `am start -W` returns at the first drawn frame; agents read the accessibility tree next,
        // so also wait until the app's window is the active one and exposes content.
        long t0 = SystemClock.uptimeMillis();
        boolean ready = false;
        while (!ready && SystemClock.uptimeMillis() - t0 < 3000) {
            ready = appWindowReady(cn.getPackageName(), prevFg);
            if (!ready) SystemClock.sleep(5);
        }
        sb.append("{\"component\":"); str(sb, cn.flattenToShortString());
        sb.append(",\"result\":").append(result).append(",\"totalTime\":").append(total);
        sb.append(",\"a11yReadyMs\":").append(SystemClock.uptimeMillis() - t0).append(",\"a11yReady\":").append(ready).append('}');
    }

    /**
     * The launched task is in front once the active application window has content and is
     * either the app's own window or a new window (a task's top activity may belong to another
     * package, e.g. Settings' search screen).
     */
    static boolean appWindowReady(String pkg, String prevFg) {
        try {
            for (AccessibilityWindowInfo w : ua.getWindows()) {
                if (w.getType() != AccessibilityWindowInfo.TYPE_APPLICATION || !(w.isActive() || w.isFocused())) continue;
                AccessibilityNodeInfo r = w.getRoot();
                if (r == null || r.getChildCount() == 0) continue;
                String p = cs(r.getPackageName());
                if (p.equals(pkg) || !p.equals(prevFg)) return true;
            }
        } catch (Throwable ignored) {}
        return false;
    }

    static void forceStop(String pkg) throws Exception {
        IBinder b = (IBinder) Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, "activity");
        Object am = Class.forName("android.app.IActivityManager$Stub").getMethod("asInterface", IBinder.class).invoke(null, b);
        am.getClass().getMethod("forceStopPackage", String.class, int.class).invoke(am, pkg, USER_CURRENT);
    }

    // ---------------------------------------------------------------- input (keys / text)
    //
    // A phone AVD (hw.keyboard=no, like a real phone) has no keyboard device, so the emulator's
    // gRPC sendKey has nothing to drive. Keys and text are injected here the way
    // `adb shell input` does (virtual keyboard device), but synchronously: injectInputEvent(sync)
    // returns after the focused app has handled the event.

    // InputManager.INJECT_INPUT_EVENT_MODE_*: the same call `adb shell input` makes (cmd input).
    // UiAutomation.injectInputEvent(e, true) is avoided on purpose: since API 33 it also waits
    // for window animations, which a soft keyboard runs on every key press.
    static final int INJECT_ASYNC = 0, INJECT_WAIT_FOR_FINISH = 2;
    static Object inputManager;
    static Method injectMethod;

    static void inject(android.view.InputEvent ev, int mode) {
        try {
            if (injectMethod == null) {
                IBinder b = (IBinder) Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, "input");
                inputManager = Class.forName("android.hardware.input.IInputManager$Stub").getMethod("asInterface", IBinder.class).invoke(null, b);
                injectMethod = inputManager.getClass().getMethod("injectInputEvent", android.view.InputEvent.class, int.class);
            }
            injectMethod.invoke(inputManager, ev, mode);
        } catch (Throwable t) {
            // Fall back to UiAutomation (needs no reflection).
            ua.injectInputEvent(ev, mode != INJECT_ASYNC);
        }
    }

    /**
     * Presses a key (down + up). async: return once InputManager has queued both events, like a
     * touch through the emulator; otherwise wait until the focused window has handled the up.
     */
    static void injectKey(int code, boolean async) {
        long now = SystemClock.uptimeMillis();
        KeyEvent down = new KeyEvent(now, now, KeyEvent.ACTION_DOWN, code, 0, 0, KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD);
        inject(down, INJECT_ASYNC);
        KeyEvent up = new KeyEvent(now, SystemClock.uptimeMillis(), KeyEvent.ACTION_UP, code, 0, 0, KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD);
        inject(up, async ? INJECT_ASYNC : INJECT_WAIT_FOR_FINISH);
    }

    /**
     * Types text. By default the text is inserted into the focused editable node with
     * ACTION_SET_TEXT at its cursor (what UiAutomator / Appium setText do): the app's
     * TextWatchers and listeners run before this returns. '\n' and '\t' are sent as the Enter and
     * Tab keys. With keys=true, or when no editable node has input focus, every character is a
     * key event through the IME path, like `adb shell input text`.
     */
    static String typeText(String text, boolean keys) {
        if (keys) {
            injectText(text);
            return "keys";
        }
        StringBuilder seg = new StringBuilder();
        boolean any = false;
        for (int i = 0; i < text.length(); i++) {
            char c = text.charAt(i);
            if (c == '\n' || c == '\r' || c == '\t') {
                if (seg.length() > 0) {
                    if (!setText(seg.toString(), any ? 300 : 1000)) {
                        injectText(text.substring(i - seg.length()));
                        return any ? "settext+keys" : "keys";
                    }
                    any = true;
                    seg.setLength(0);
                }
                if (c == '\r' && i + 1 < text.length() && text.charAt(i + 1) == '\n') continue;
                injectKey(c == '\t' ? KeyEvent.KEYCODE_TAB : KeyEvent.KEYCODE_ENTER, false);
                continue;
            }
            seg.append(c);
        }
        if (seg.length() > 0) {
            if (!setText(seg.toString(), any ? 300 : 1000)) {
                injectText(seg.toString());
                return any ? "settext+keys" : "keys";
            }
            any = true;
        }
        return any ? "settext" : "keys";
    }

    /** The node with input focus if it is editable, waiting up to timeoutMs for one. */
    static AccessibilityNodeInfo focusedEditable(long timeoutMs) {
        long t0 = SystemClock.uptimeMillis();
        while (true) {
            AccessibilityNodeInfo f = null;
            try { f = ua.findFocus(AccessibilityNodeInfo.FOCUS_INPUT); } catch (Throwable ignored) {}
            if (f != null && f.isEditable()) return f;
            if (SystemClock.uptimeMillis() - t0 >= timeoutMs) return null;
            SystemClock.sleep(10);
        }
    }

    /** Inserts text at the cursor of the focused editable node. False: not possible (use keys). */
    static boolean setText(String text, long focusWaitMs) {
        AccessibilityNodeInfo f = focusedEditable(focusWaitMs);
        if (f == null) return false;
        String cur = f.isShowingHintText() ? "" : cs(f.getText());
        // A password field exposes masked text; it can only be set when it is empty.
        if (f.isPassword() && !cur.isEmpty()) return false;
        int a = f.getTextSelectionStart(), b = f.getTextSelectionEnd();
        if (a < 0 || b < 0 || a > cur.length() || b > cur.length()) a = b = cur.length();
        if (a > b) { int t = a; a = b; b = t; }
        String next = cur.substring(0, a) + text + cur.substring(b);
        Bundle args = new Bundle();
        args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, next);
        if (!f.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args)) return false;
        int caret = a + text.length();
        if (caret != next.length()) { // SET_TEXT leaves the cursor at the end; put it after the insert
            Bundle sel = new Bundle();
            sel.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_START_INT, caret);
            sel.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_END_INT, caret);
            f.performAction(AccessibilityNodeInfo.ACTION_SET_SELECTION, sel);
        }
        return true;
    }

    /** Types text with virtual-keyboard key events (like `input text`); characters with no key use ACTION_MULTIPLE. */
    static void injectText(String text) {
        KeyCharacterMap kcm = KeyCharacterMap.load(KeyCharacterMap.VIRTUAL_KEYBOARD);
        List<KeyEvent> all = new ArrayList<>();
        int i = 0;
        while (i < text.length()) {
            int cp = text.codePointAt(i);
            String ch = new String(Character.toChars(cp));
            i += Character.charCount(cp);
            KeyEvent[] evs = kcm.getEvents(ch.toCharArray());
            if (evs != null) {
                for (KeyEvent e : evs) all.add(e);
            } else {
                all.add(new KeyEvent(SystemClock.uptimeMillis(), ch, KeyCharacterMap.VIRTUAL_KEYBOARD, 0));
            }
        }
        // Typing right after a tap can race the keyboard binding to the new field (Gboard then
        // re-applies its composing text and characters appear twice). Wait until an editable
        // view has input focus, then send each event and wait until the app handled it, as
        // `input text` does.
        long t0 = SystemClock.uptimeMillis();
        while (SystemClock.uptimeMillis() - t0 < 2000) {
            AccessibilityNodeInfo f = null;
            try { f = ua.findFocus(AccessibilityNodeInfo.FOCUS_INPUT); } catch (Throwable ignored) {}
            if (f != null && f.isEditable()) break;
            SystemClock.sleep(10);
        }
        for (KeyEvent e : all) {
            KeyEvent ev = e.getAction() == KeyEvent.ACTION_MULTIPLE ? e : KeyEvent.changeTimeRepeat(e, SystemClock.uptimeMillis(), 0);
            ev.setSource(InputDevice.SOURCE_KEYBOARD);
            inject(ev, INJECT_WAIT_FOR_FINISH);
        }
    }

    // ---------------------------------------------------------------- helpers

    static String sysprop(String k) {
        try {
            return (String) Class.forName("android.os.SystemProperties").getMethod("get", String.class).invoke(null, k);
        } catch (Throwable e) {
            return "";
        }
    }

    static String cs(CharSequence c) { return c == null ? "" : c.toString(); }

    static void rect(StringBuilder sb, Rect r) {
        sb.append(r.left).append(',').append(r.top).append(',').append(r.right).append(',').append(r.bottom);
    }

    static void str(StringBuilder sb, String s) {
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"': sb.append("\\\""); break;
                case '\\': sb.append("\\\\"); break;
                case '\n': sb.append("\\n"); break;
                case '\r': sb.append("\\r"); break;
                case '\t': sb.append("\\t"); break;
                default:
                    if (c < 0x20 || c == 0x2028 || c == 0x2029) sb.append(String.format("\\u%04x", (int) c));
                    else sb.append(c);
            }
        }
        sb.append('"');
    }
}
