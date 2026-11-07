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
                awaitPendingLaunch();
                synchronized (uaLock) { tree(sb); }
                return;
            case "profile":
                profile(req.getInt("w"), req.getInt("h"), req.getInt("density"), req.optLong("settleMs", 3000));
                display(sb);
                return;
            case "idle":
                sb.append(idle(req.optLong("quietMs", 100), req.optLong("timeoutMs", 2000)) ? "true" : "false");
                return;
            case "settle": {
                long t0 = SystemClock.uptimeMillis();
                boolean settled = awaitWindowsSettled(req.optLong("maxMs", 2000));
                sb.append("{\"settled\":").append(settled);
                sb.append(",\"ms\":").append(SystemClock.uptimeMillis() - t0).append('}');
                return;
            }
            case "clearcache":
                // The next tree read asks the apps for every node again. The accessibility cache
                // keeps a node's bounds until an event about it arrives, and moving content does
                // not always send one (a list scrolling, a permission dialog moving into place):
                // read through the cache, a moved element can stay where it was for 400 ms and more.
                synchronized (uaLock) { sb.append(ua.clearCache() ? "true" : "false"); }
                return;
            case "launch":
                launch(req.getString("pkg"), req.optString("activity", ""), sb);
                return;
            case "stop":
                forceStop(req.getString("pkg"));
                sb.append("true");
                return;
            case "text": {
                tFind = tFocus = tSet = tSel = 0;
                long t0 = SystemClock.uptimeMillis();
                String mode = typeText(req.getString("text"), req.optBoolean("keys", false));
                sb.append("{\"mode\":"); str(sb, mode);
                sb.append(",\"ms\":").append(SystemClock.uptimeMillis() - t0);
                sb.append(",\"selMs\":").append(tSel).append(",\"sel\":"); str(sb, lastSel);
                sb.append(",\"findMs\":").append(tFind).append(",\"focusMs\":").append(tFocus).append(",\"setMs\":").append(tSet).append('}');
                return;
            }
            case "key":
                injectKey(req.getInt("code"), req.optBoolean("async", false));
                sb.append("true");
                return;
            case "fg":
                str(sb, foreground());
                return;
            case "awaitlaunch":
                sb.append(awaitPendingLaunch() ? "true" : "false");
                return;
            case "cleartasks": {
                // Remove recent tasks (what swiping everything away in Overview does).
                Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
                atm.getClass().getMethod("removeAllVisibleRecentTasks").invoke(atm);
                sb.append("true");
                return;
            }
            // Test capabilities (Caps.java, docs/CAPABILITIES.md).
            case "openurl":
                Caps.openURL(req.getString("url"), sb);
                return;
            case "permstate":
                Caps.permState(req.getString("pkg"), req.optJSONArray("only"), sb);
                return;
            case "perms":
                Caps.applyPerms(req, sb);
                return;
            case "clipget":
                str(sb, Caps.clipText());
                return;
            case "clipset":
                Caps.setClip(req.getString("text"));
                sb.append("true");
                return;
            case "appearance":
                if (req.optString("mode").isEmpty()) {
                    sb.append("{\"appearance\":"); str(sb, Caps.appearance()); sb.append('}');
                } else {
                    Caps.setAppearance(req.getString("mode"), req.optLong("settleMs", 2000), req.optLong("graceMs", 300), sb);
                }
                return;
            case "apps":
                Caps.apps(sb);
                return;
            case "uninstall":
                Caps.uninstall(req.getString("pkg"), sb);
                return;
            case "clear":
                Caps.clearData(req.getString("pkg"), sb);
                return;
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

    /** The settle in progress (null: none); later callers wait for it instead of starting another. */
    static java.util.concurrent.CountDownLatch settling;
    static boolean settledOK;

    /**
     * Waits, at most maxMs, until WindowManager runs no window animation or transition (an
     * activity or a dialog opening or closing, a permission dialog sliding in) and has handed
     * the resulting input windows to the input dispatcher: IWindowManager.syncInputTransactions
     * (true), which UiAutomation also waits for before an injected event since API 33. Until
     * then a touch can go to a window that is leaving (a closing dialog takes every touch while
     * its exit animation runs) or land at a place the window is only moving to.
     *
     * WindowManager bounds its own wait at 5 s and cannot be interrupted, so it runs on a thread:
     * when maxMs passes first, this returns false and the thread ends on its own; a call meanwhile
     * waits for the same thread instead of starting another.
     */
    static boolean awaitWindowsSettled(long maxMs) {
        final java.util.concurrent.CountDownLatch done;
        synchronized (Agent.class) {
            if (settling == null) {
                settling = new java.util.concurrent.CountDownLatch(1);
                final java.util.concurrent.CountDownLatch mine = settling;
                Thread t = new Thread(new Runnable() {
                    public void run() {
                        boolean ok;
                        try {
                            Object wm = windowManager();
                            Object r = wm.getClass().getMethod("syncInputTransactions", boolean.class).invoke(wm, true);
                            ok = !(r instanceof Boolean) || (Boolean) r;
                        } catch (Throwable e) {
                            ok = false;
                        }
                        synchronized (Agent.class) {
                            settledOK = ok;
                            settling = null;
                        }
                        mine.countDown();
                    }
                }, "agemu-settle");
                t.setDaemon(true);
                t.start();
            }
            done = settling;
        }
        try {
            if (!done.await(maxMs, java.util.concurrent.TimeUnit.MILLISECONDS)) return false;
        } catch (InterruptedException e) {
            return false;
        }
        synchronized (Agent.class) {
            return settledOK;
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

    /** A launch whose window the accessibility layer may not report yet (see awaitPendingLaunch). */
    static final Object launchLock = new Object();
    static String pendingPkg, pendingPrevFg;
    static long pendingUntil;

    /**
     * Starts the app like `am start -W` (the same startActivityAndWait call) and returns when it
     * does: once the activity has drawn its first frame. The accessibility layer reports the new
     * window only after the window transition; the next "tree" request waits for that
     * (awaitPendingLaunch), so launch + ui stays consistent without slowing launch itself.
     */
    static void launch(String pkg, String activity, StringBuilder sb) throws Exception {
        long t0 = SystemClock.uptimeMillis();
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
        long t1 = SystemClock.uptimeMillis();
        String prevFg = foreground();
        long t2 = SystemClock.uptimeMillis();
        Intent i = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER).setComponent(cn)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
        Method sm = null;
        for (Method m : atm.getClass().getMethods()) if (m.getName().equals("startActivityAndWait")) sm = m;
        if (sm == null) throw new IllegalStateException("startActivityAndWait not found");
        // (caller, callingPackage, callingFeatureId, intent, resolvedType, resultTo, resultWho,
        //  requestCode, flags, profilerInfo, options, userId) - the same call `am start -W` makes.
        Object wr = startAndWait(atm, sm, cn.flattenToShortString(), null, "com.android.shell", null, i, null, null, null, 0, 0, null, null, USER_CURRENT);
        long t3 = SystemClock.uptimeMillis();
        int result = wr.getClass().getField("result").getInt(wr);
        long total = wr.getClass().getField("totalTime").getLong(wr);
        if (result < 0) throw new IllegalStateException("start failed, result " + result + " for " + cn.flattenToShortString());
        setPendingLaunch(cn.getPackageName(), prevFg, t3);
        sb.append("{\"component\":"); str(sb, cn.flattenToShortString());
        sb.append(",\"result\":").append(result).append(",\"totalTime\":").append(total);
        sb.append(",\"resolveMs\":").append(t1 - t0).append(",\"fgMs\":").append(t2 - t1);
        sb.append(",\"startMs\":").append(t3 - t2).append('}');
    }

    /** Uptime of the last key sent without waiting for its effect ("key" with async). */
    static volatile long lastAsyncKey;
    /** How long a start waits after such a key, so the key's effect lands first. */
    static final long KEY_SETTLE_MS = 300;
    /** Upper bound of a startActivityAndWait (a cold start on a loaded host takes seconds). */
    static final long START_WAIT_MS = 20000;
    /**
     * Starts that passed START_WAIT_MS and are still blocked in startActivityAndWait. Each one
     * holds a thread here and a binder thread in system_server, so after MAX_STUCK_STARTS new
     * starts are refused until one returns. `agemu reset` restores the agent and system_server
     * from the baseline, which clears them.
     */
    static final java.util.concurrent.atomic.AtomicInteger stuckStarts = new java.util.concurrent.atomic.AtomicInteger();
    static final int MAX_STUCK_STARTS = 3;

    /**
     * startActivityAndWait (args as for ActivityTaskManager's), safe against two hazards:
     *   - a start right after an async key (home, back) races the key: when the key is handled
     *     after the start, the new activity goes behind and never reports being displayed, and
     *     startActivityAndWait does not return (seen 3 of 6 times with home sent 1 ms before; never
     *     with 300 ms between). So the start waits until KEY_SETTLE_MS after such a key.
     *   - any other start that never shows is bounded by START_WAIT_MS, with an error.
     */
    static Object startAndWait(final Object atm, final Method sm, String what, final Object... args) throws Exception {
        if (stuckStarts.get() >= MAX_STUCK_STARTS) {
            throw new IllegalStateException(stuckStarts.get() + " earlier starts never reported being displayed and still hold a"
                    + " system thread; not starting " + what + " (agemu reset clears them)");
        }
        long since = SystemClock.uptimeMillis() - lastAsyncKey;
        if (since >= 0 && since < KEY_SETTLE_MS) SystemClock.sleep(KEY_SETTLE_MS - since);
        final Object[] box = new Object[2];
        final java.util.concurrent.CountDownLatch done = new java.util.concurrent.CountDownLatch(1);
        // Set by the waiter when it gives up: the thread then counts itself out when it returns.
        final java.util.concurrent.atomic.AtomicBoolean abandoned = new java.util.concurrent.atomic.AtomicBoolean();
        Thread t = new Thread(new Runnable() {
            public void run() {
                try { box[0] = sm.invoke(atm, args); } catch (Throwable e) { box[1] = e; }
                synchronized (abandoned) {
                    done.countDown();
                    if (abandoned.get()) stuckStarts.decrementAndGet();
                }
            }
        }, "agemu-start");
        t.setDaemon(true);
        t.start();
        if (!done.await(START_WAIT_MS, java.util.concurrent.TimeUnit.MILLISECONDS)) {
            synchronized (abandoned) {
                if (done.getCount() > 0) {
                    abandoned.set(true);
                    stuckStarts.incrementAndGet();
                }
            }
            if (abandoned.get()) {
                throw new IllegalStateException("started " + what + ", but it did not report being displayed in "
                        + START_WAIT_MS / 1000 + " s (in front: " + foreground() + ")");
            }
        }
        if (box[1] instanceof java.lang.reflect.InvocationTargetException) throw (Exception) ((Throwable) box[1]).getCause();
        if (box[1] instanceof Exception) throw (Exception) box[1];
        if (box[1] != null) throw new RuntimeException((Throwable) box[1]);
        return box[0];
    }

    /** Makes the next "tree" wait (up to 3 s after t) for pkg's window (see awaitPendingLaunch). */
    static void setPendingLaunch(String pkg, String prevFg, long t) {
        if (pkg == null || pkg.isEmpty()) return;
        synchronized (launchLock) {
            pendingPkg = pkg;
            pendingPrevFg = prevFg;
            pendingUntil = t + 3000;
        }
    }

    /** Id of the focused application window (-1: none). */
    static int fgWindowId() {
        try {
            for (AccessibilityWindowInfo w : ua.getWindows()) {
                if (w.getType() == AccessibilityWindowInfo.TYPE_APPLICATION && (w.isActive() || w.isFocused())) return w.getId();
            }
        } catch (Throwable ignored) {}
        return -1;
    }

    /**
     * After a configuration change: waits until the focused application window is a new one with
     * content (the activity was recreated and has laid out), up to timeoutMs. When no new window
     * shows within graceMs, the app handled the change in its window (or has none in front).
     * Returns "recreated", "in-place" or "timeout".
     */
    static String awaitWindowReplaced(int oldId, long graceMs, long timeoutMs) {
        long t0 = SystemClock.uptimeMillis();
        boolean gone = false;
        while (true) {
            long el = SystemClock.uptimeMillis() - t0;
            try {
                int cur = -1;
                boolean content = false;
                for (AccessibilityWindowInfo w : ua.getWindows()) {
                    if (w.getType() != AccessibilityWindowInfo.TYPE_APPLICATION || !(w.isActive() || w.isFocused())) continue;
                    cur = w.getId();
                    AccessibilityNodeInfo r = w.getRoot();
                    content = r != null && r.getChildCount() > 0;
                    break;
                }
                if (cur != oldId) gone = true;
                if (gone && cur != -1 && content) return "recreated";
            } catch (Throwable ignored) {}
            if (!gone && el >= graceMs) return "in-place";
            if (el >= timeoutMs) return "timeout";
            SystemClock.sleep(5);
        }
    }

    /**
     * Waits (up to 3 s after the launch) until the accessibility layer reports the launched app's
     * window as the active one. Returns whether a launch was pending.
     */
    static boolean awaitPendingLaunch() {
        String pkg, prev;
        long until;
        synchronized (launchLock) {
            pkg = pendingPkg; prev = pendingPrevFg; until = pendingUntil;
        }
        if (pkg == null) return false;
        while (SystemClock.uptimeMillis() < until && !appWindowReady(pkg, prev)) SystemClock.sleep(5);
        synchronized (launchLock) {
            if (pendingUntil == until) pendingPkg = null;
        }
        return true;
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
        if (async) lastAsyncKey = SystemClock.uptimeMillis();
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

    static long tFind, tFocus, tSet, tSel; // phase timings of the last setText (reported by "text")
    static String lastSel = "";

    static Method aicGetInstance, aicFind;
    static Field fConn, fWin, fSrc;

    /**
     * Re-reads a node from the app, bypassing the cache and without prefetching its relatives:
     * AccessibilityNodeInfo.refresh() asks the app to prefetch siblings and descendants on its
     * UI thread, which costs tens of milliseconds on a big form. Falls back to refresh().
     */
    static AccessibilityNodeInfo fresh(AccessibilityNodeInfo n) {
        try {
            if (aicFind == null) {
                Class<?> c = Class.forName("android.view.accessibility.AccessibilityInteractionClient");
                aicGetInstance = c.getMethod("getInstance");
                aicFind = c.getMethod("findAccessibilityNodeInfoByAccessibilityId", int.class, int.class, long.class, boolean.class, int.class, Bundle.class);
                fConn = AccessibilityNodeInfo.class.getDeclaredField("mConnectionId");
                fWin = AccessibilityNodeInfo.class.getDeclaredField("mWindowId");
                fSrc = AccessibilityNodeInfo.class.getDeclaredField("mSourceNodeId");
                fConn.setAccessible(true);
                fWin.setAccessible(true);
                fSrc.setAccessible(true);
            }
            Object client = aicGetInstance.invoke(null);
            AccessibilityNodeInfo r = (AccessibilityNodeInfo) aicFind.invoke(client, fConn.getInt(n), fWin.getInt(n), fSrc.getLong(n), true, 0, null);
            if (r != null) return r;
        } catch (Throwable ignored) {
        }
        n.refresh(); // the caller treats the same object as "not re-read" where that matters
        return n;
    }

    /** Inserts text at the cursor of the focused editable node. False: not possible (use keys). */
    static AccessibilityNodeInfo lastEdit; // node of the last setText

    static boolean setText(String text, long focusWaitMs) {
        long t0 = SystemClock.uptimeMillis();
        // Typing usually continues in the field typed into last: re-read just that node (one
        // round trip to the app, no prefetching) and keep it if it still has input focus.
        // findFocus misses the cache after every text change and costs more.
        AccessibilityNodeInfo f = null;
        if (lastEdit != null) {
            AccessibilityNodeInfo r = fresh(lastEdit);
            if (r != lastEdit && r.isFocused() && r.isEditable()) f = r;
        }
        if (f == null) {
            f = focusedEditable(focusWaitMs);
            if (f == null) {
                tFocus += SystemClock.uptimeMillis() - t0;
                return false;
            }
            tFind += SystemClock.uptimeMillis() - t0;
            // The node may come from the accessibility cache, which the app updates
            // asynchronously after each change: read the current text and cursor from the app,
            // or back-to-back calls would insert at a stale position.
            f = fresh(f);
        }
        lastEdit = f;
        tFocus += SystemClock.uptimeMillis() - t0;
        String cur = f.isShowingHintText() ? "" : cs(f.getText());
        // A password field exposes masked text; it can only be set when it is empty.
        if (f.isPassword() && !cur.isEmpty()) return false;
        int a = f.getTextSelectionStart(), b = f.getTextSelectionEnd();
        if (a < 0 || b < 0 || a > cur.length() || b > cur.length()) a = b = cur.length();
        if (a > b) { int t = a; a = b; b = t; }
        String next = cur.substring(0, a) + text + cur.substring(b);
        Bundle args = new Bundle();
        args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, next);
        long t1 = SystemClock.uptimeMillis();
        boolean done = f.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args);
        tSet += SystemClock.uptimeMillis() - t1;
        if (!done) return false;
        int caret = a + text.length();
        lastSel = a + "," + b + "/" + cur.length();
        if (caret != next.length()) { // SET_TEXT leaves the cursor at the end; put it after the insert
            long t2 = SystemClock.uptimeMillis();
            Bundle sel = new Bundle();
            sel.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_START_INT, caret);
            sel.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_END_INT, caret);
            f.performAction(AccessibilityNodeInfo.ACTION_SET_SELECTION, sel);
            tSel += SystemClock.uptimeMillis() - t2;
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
