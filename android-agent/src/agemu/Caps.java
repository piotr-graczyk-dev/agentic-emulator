package agemu;

import android.content.ClipData;
import android.content.ComponentName;
import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageInfo;
import android.content.pm.PackageManager;
import android.content.pm.PermissionInfo;
import android.content.res.AssetManager;
import android.content.res.Configuration;
import android.content.res.Resources;
import android.net.Uri;
import android.os.Binder;
import android.os.IBinder;
import android.os.Parcel;
import android.os.SystemClock;
import android.util.DisplayMetrics;

import org.json.JSONArray;
import org.json.JSONObject;

import java.lang.reflect.Field;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Test capabilities (docs/CAPABILITIES.md): open a URL, permissions, clipboard, night mode, user
 * apps, uninstall and clear data. Each one is the system service call the matching user action
 * or shell command makes, made from the agent's process (the shell uid) instead of a new
 * process per call. The host plans permission changes (internal/android/capabilities.go); this
 * side only reads state and applies the plan.
 */
final class Caps {
    static final String SHELL = "com.android.shell";
    static final int USER_SYSTEM = 0;

    // ---------------------------------------------------------------- binder plumbing

    static Object service(String name, String iface) throws Exception {
        IBinder b = (IBinder) Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, name);
        if (b == null) throw new IllegalStateException("no system service " + name);
        return Class.forName(iface + "$Stub").getMethod("asInterface", IBinder.class).invoke(null, b);
    }

    static Method method(Object o, String name) {
        Method best = null;
        for (Method m : o.getClass().getMethods()) {
            if (!m.getName().equals(name)) continue;
            if (best == null || m.getParameterTypes().length > best.getParameterTypes().length) best = m;
        }
        if (best == null) throw new IllegalStateException(o.getClass().getName() + "." + name + " not found");
        return best;
    }

    /**
     * Arguments for a hidden service method whose signature changes between releases by gaining
     * trailing ids: Strings are taken in order from strs (missing ones are null, e.g. an
     * attribution tag; a String device id gets the default device), ints from ints (missing ones
     * are 0, e.g. the default device id), longs and booleans from the first given values.
     */
    static Object[] args(Method m, Object first, String[] strs, int[] ints, long flags, boolean bool) {
        Class<?>[] ps = m.getParameterTypes();
        Object[] a = new Object[ps.length];
        int si = 0, ii = 0, start = 0;
        if (first != null) { a[0] = first; start = 1; }
        for (int k = start; k < ps.length; k++) {
            Class<?> p = ps[k];
            if (p == String.class) a[k] = si < strs.length ? strs[si++] : (si++ == strs.length && k == ps.length - 2 ? "default:0" : null);
            else if (p == int.class) a[k] = ii < ints.length ? ints[ii++] : 0;
            else if (p == long.class) a[k] = flags;
            else if (p == boolean.class) a[k] = bool;
            else a[k] = null;
        }
        return a;
    }

    static Object call(Object svc, String name, Object first, String[] strs, int[] ints, long flags, boolean bool) throws Exception {
        Method m = method(svc, name);
        return m.invoke(svc, args(m, first, strs, ints, flags, bool));
    }

    /** A binder for a one-way callback interface (hidden at compile time) that records one call. */
    static final class Callback extends Binder {
        final String descriptor;
        final CountDownLatch done = new CountDownLatch(1);
        volatile String str;
        volatile int num;

        Callback(String descriptor) {
            this.descriptor = descriptor;
            attachInterface(null, descriptor);
        }

        @Override
        protected boolean onTransact(int code, Parcel data, Parcel reply, int flags) throws android.os.RemoteException {
            if (code == FIRST_CALL_TRANSACTION) {
                data.enforceInterface(descriptor);
                str = data.readString();
                num = data.readInt(); // int return code, or a boolean (written as an int)
                done.countDown();
                return true;
            }
            return super.onTransact(code, data, reply, flags);
        }

        /** The callback as the hidden interface type, for a reflective call. */
        Object asInterface() throws Exception {
            final Class<?> iface = Class.forName(descriptor);
            final Binder self = this;
            return Proxy.newProxyInstance(iface.getClassLoader(), new Class<?>[]{iface}, new InvocationHandler() {
                public Object invoke(Object p, Method m, Object[] a) {
                    if (m.getName().equals("asBinder")) return self;
                    if (m.getName().equals("hashCode")) return System.identityHashCode(p);
                    if (m.getName().equals("equals")) return p == a[0];
                    if (m.getName().equals("toString")) return descriptor;
                    return null;
                }
            });
        }

        boolean await(long ms) throws InterruptedException { return done.await(ms, TimeUnit.MILLISECONDS); }
    }

    /** Name of a static int constant of cls with the given prefix and value, or the value. */
    static String constName(Class<?> cls, String prefix, int value) {
        for (Field f : cls.getFields()) {
            try {
                if (f.getName().startsWith(prefix) && f.getType() == int.class && f.getInt(null) == value) return f.getName();
            } catch (Throwable ignored) {}
        }
        return prefix + value;
    }

    // ---------------------------------------------------------------- packages

    static Object pm() throws Exception {
        return Class.forName("android.app.ActivityThread").getMethod("getPackageManager").invoke(null);
    }

    static PackageInfo packageInfo(String pkg, long flags) throws Exception {
        Object pi = call(pm(), "getPackageInfo", pkg, new String[]{}, new int[]{USER_SYSTEM}, flags, false);
        if (pi == null) throw new IllegalArgumentException(pkg + " is not installed");
        return (PackageInfo) pi;
    }

    static boolean isSystem(ApplicationInfo ai) { return (ai.flags & ApplicationInfo.FLAG_SYSTEM) != 0; }

    /** Labels already read, by APK path, update time and locale (the agent is long-lived). */
    static final Map<String, String> labels = new java.util.HashMap<>();

    /**
     * Display label of an installed app, read from its APK's resources in the device's current
     * configuration (its locale, as the launcher shows it), or "" when unknown. Each APK is read
     * once per update and locale; later calls take the label from the cache.
     */
    static String label(PackageInfo p, Configuration cfg) {
        ApplicationInfo ai = p.applicationInfo;
        try {
            if (ai.nonLocalizedLabel != null) return ai.nonLocalizedLabel.toString();
            if (ai.labelRes == 0) return "";
            String key = ai.sourceDir + "|" + p.lastUpdateTime + "|" + (cfg == null ? "" : cfg.getLocales().toLanguageTags());
            synchronized (labels) {
                String l = labels.get(key);
                if (l != null) return l;
            }
            AssetManager am = AssetManager.class.getConstructor().newInstance();
            AssetManager.class.getMethod("addAssetPath", String.class).invoke(am, ai.sourceDir);
            Resources r = new Resources(am, new DisplayMetrics(), cfg == null ? new Configuration() : cfg);
            CharSequence t = r.getText(ai.labelRes);
            am.close();
            String l = t == null ? "" : t.toString();
            synchronized (labels) {
                if (labels.size() > 500) labels.clear();
                labels.put(key, l);
            }
            return l;
        } catch (Throwable e) {
            return "";
        }
    }

    /** The global configuration apps get (locale, night mode), or null when it cannot be read. */
    static Configuration globalConfig() {
        try {
            Object am = service("activity", "android.app.IActivityManager");
            return (Configuration) method(am, "getConfiguration").invoke(am);
        } catch (Throwable e) {
            return null;
        }
    }

    /** User apps (not part of the system image), sorted by package name. */
    static void apps(StringBuilder sb) throws Exception {
        Object slice = call(pm(), "getInstalledPackages", null, new String[]{}, new int[]{USER_SYSTEM}, 0L, false);
        @SuppressWarnings("unchecked")
        List<PackageInfo> all = (List<PackageInfo>) slice.getClass().getMethod("getList").invoke(slice);
        List<PackageInfo> user = new ArrayList<>();
        for (PackageInfo p : all) if (p.applicationInfo != null && !isSystem(p.applicationInfo)) user.add(p);
        Collections.sort(user, new Comparator<PackageInfo>() {
            public int compare(PackageInfo a, PackageInfo b) { return a.packageName.compareTo(b.packageName); }
        });
        Configuration cfg = globalConfig();
        sb.append('[');
        for (int i = 0; i < user.size(); i++) {
            PackageInfo p = user.get(i);
            if (i > 0) sb.append(',');
            sb.append("{\"id\":"); Agent.str(sb, p.packageName);
            String l = label(p, cfg);
            if (!l.isEmpty()) { sb.append(",\"name\":"); Agent.str(sb, l); }
            if (p.versionName != null) { sb.append(",\"version\":"); Agent.str(sb, p.versionName); }
            sb.append('}');
        }
        sb.append(']');
    }

    /** Uninstalls a user app for all users, as `pm uninstall` does. */
    static void uninstall(String pkg, StringBuilder sb) throws Exception {
        PackageInfo pi = packageInfo(pkg, 0);
        if (isSystem(pi.applicationInfo)) throw new IllegalArgumentException(pkg + " is a system app; only user apps can be uninstalled");
        Callback cb = new Callback("android.content.pm.IPackageDeleteObserver");
        int deleteAllUsers = PackageManager.class.getField("DELETE_ALL_USERS").getInt(null);
        Object pm = pm();
        Method m = method(pm, "deletePackageAsUser"); // (pkg, versionCode, observer, userId, flags)
        m.invoke(pm, pkg, -1 /* VERSION_CODE_HIGHEST */, cb.asInterface(), USER_SYSTEM, deleteAllUsers);
        if (!cb.await(120000)) throw new IllegalStateException("uninstall of " + pkg + " did not finish in 120 s");
        if (cb.num != 1 /* DELETE_SUCCEEDED */) {
            throw new IllegalStateException("uninstall " + pkg + ": " + constName(PackageManager.class, "DELETE_", cb.num));
        }
        sb.append("true");
    }

    /** Settings' "Clear storage" (`pm clear`): stops the app, deletes its data, resets its permissions. */
    static void clearData(String pkg, StringBuilder sb) throws Exception {
        packageInfo(pkg, 0);
        Callback cb = new Callback("android.content.pm.IPackageDataObserver");
        Object am = service("activity", "android.app.IActivityManager");
        Method m = method(am, "clearApplicationUserData"); // (pkg, keepState, observer, userId)
        Object ok = m.invoke(am, pkg, false, cb.asInterface(), USER_SYSTEM);
        if (Boolean.FALSE.equals(ok)) throw new IllegalStateException("clearing the data of " + pkg + " was refused");
        if (!cb.await(120000)) throw new IllegalStateException("clearing the data of " + pkg + " did not finish in 120 s");
        if (cb.num == 0) throw new IllegalStateException("clearing the data of " + pkg + " failed");
        sb.append("true");
    }

    // ---------------------------------------------------------------- permissions

    static final String[] FLAG_NAMES = {
        "USER_SET", "USER_FIXED", "POLICY_FIXED", "SYSTEM_FIXED", "GRANTED_BY_DEFAULT", "GRANTED_BY_ROLE",
        "REVIEW_REQUIRED", "REVOKE_WHEN_REQUESTED", "REVOKED_COMPAT", "ONE_TIME", "AUTO_REVOKED",
        "USER_SENSITIVE_WHEN_GRANTED", "USER_SENSITIVE_WHEN_DENIED", "SELECTED_LOCATION_ACCURACY",
        "RESTRICTION_INSTALLER_EXEMPT", "RESTRICTION_SYSTEM_EXEMPT", "RESTRICTION_UPGRADE_EXEMPT",
        "APPLY_RESTRICTION",
    };
    static Map<String, Integer> flagBits;

    static synchronized Map<String, Integer> flagBits() {
        if (flagBits != null) return flagBits;
        Map<String, Integer> m = new LinkedHashMap<>();
        for (String n : FLAG_NAMES) {
            try { m.put(n, PackageManager.class.getField("FLAG_PERMISSION_" + n).getInt(null)); } catch (Throwable ignored) {}
        }
        flagBits = m;
        return m;
    }

    static int flagMask(JSONArray names) throws Exception {
        int v = 0;
        if (names == null) return 0;
        for (int i = 0; i < names.length(); i++) {
            Integer b = flagBits().get(names.getString(i));
            if (b == null) throw new IllegalArgumentException("unknown permission flag " + names.getString(i));
            v |= b;
        }
        return v;
    }

    static void flagList(StringBuilder sb, int flags) {
        sb.append('[');
        boolean first = true;
        for (Map.Entry<String, Integer> e : flagBits().entrySet()) {
            if ((flags & e.getValue()) == 0) continue;
            if (!first) sb.append(',');
            first = false;
            Agent.str(sb, e.getKey());
            flags &= ~e.getValue();
        }
        if (flags != 0) { if (!first) sb.append(','); Agent.str(sb, "0x" + Integer.toHexString(flags)); }
        sb.append(']');
    }

    static Object permMgr() throws Exception { return service("permissionmgr", "android.permission.IPermissionManager"); }

    static int permFlags(Object pmgr, String pkg, String perm) throws Exception {
        return (Integer) call(pmgr, "getPermissionFlags", pkg, new String[]{perm}, new int[]{USER_SYSTEM}, 0, false);
    }

    static void updateFlags(Object pmgr, String pkg, String perm, int mask, int values) throws Exception {
        if (mask == 0) return;
        Method m = method(pmgr, "updatePermissionFlags");
        // (pkg, perm, flagMask, flagValues, checkAdjustPolicyFlagPermission[, deviceId], userId)
        Class<?>[] ps = m.getParameterTypes();
        Object[] a = new Object[ps.length];
        a[0] = pkg; a[1] = perm; a[2] = mask; a[3] = values; a[4] = false;
        if (ps.length == 7) a[5] = ps[5] == String.class ? (Object) "default:0" : (Object) 0;
        a[ps.length - 1] = USER_SYSTEM;
        m.invoke(pmgr, a);
    }

    static Object appOps() throws Exception { return service("appops", "com.android.internal.app.IAppOpsService"); }

    static int opCode(String op) throws Exception {
        return (Integer) Class.forName("android.app.AppOpsManager").getMethod("strOpToOp", String.class).invoke(null, op);
    }

    static final String[] MODES = {"allowed", "ignored", "errored", "default", "foreground"};

    static String modeName(int m) { return m >= 0 && m < MODES.length ? MODES[m] : String.valueOf(m); }

    /**
     * The app's requested permissions with what the host needs to plan a change: granted, flags,
     * runtime (dangerous) or app-op (special access, e.g. SYSTEM_ALERT_WINDOW) and its mode.
     */
    /** A permission's protection: base level, and its app-op when it is an app-op permission. */
    static final class Prot {
        String level = "unknown"; // dangerous (runtime), normal, signature, internal, unknown
        String op;                // app-op permissions (special access)
    }

    /**
     * Protections of platform permissions (android.permission.*), which only change with the
     * system image. Permissions that apps define are read every time (an app update may change
     * them).
     */
    static final Map<String, Prot> protCache = new java.util.concurrent.ConcurrentHashMap<>();

    static Prot protection(Object pmgr, String perm) {
        Prot p = protCache.get(perm);
        if (p != null) return p;
        p = new Prot();
        try {
            PermissionInfo info = (PermissionInfo) call(pmgr, "getPermissionInfo", perm, new String[]{SHELL}, new int[]{}, 0, false);
            if (info != null) {
                int base = info.getProtection();
                p.level = base == PermissionInfo.PROTECTION_DANGEROUS ? "dangerous"
                    : base == PermissionInfo.PROTECTION_NORMAL ? "normal"
                    : base == PermissionInfo.PROTECTION_SIGNATURE ? "signature" : "internal";
                if ((info.getProtectionFlags() & PermissionInfo.PROTECTION_FLAG_APPOP) != 0) {
                    p.op = (String) Class.forName("android.app.AppOpsManager").getMethod("permissionToOp", String.class).invoke(null, perm);
                }
                if ("android".equals(info.packageName)) protCache.put(perm, p);
            }
        } catch (Throwable ignored) {}
        return p;
    }

    /**
     * Grant state and flags of all the app's runtime permissions in one call
     * (IPermissionManager.getAllPermissionStates, Android 15+), or null when unavailable.
     */
    static Map<String, int[]> allPermissionStates(Object pmgr, String pkg) {
        try {
            Map<?, ?> all = (Map<?, ?>) call(pmgr, "getAllPermissionStates", pkg, new String[]{}, new int[]{USER_SYSTEM}, 0, false);
            if (all == null) return null;
            Map<String, int[]> out = new java.util.HashMap<>();
            for (Map.Entry<?, ?> e : all.entrySet()) {
                Object st = e.getValue();
                boolean g = (Boolean) st.getClass().getMethod("isGranted").invoke(st);
                int f = (Integer) st.getClass().getMethod("getFlags").invoke(st);
                out.put((String) e.getKey(), new int[]{g ? 1 : 0, f});
            }
            return out;
        } catch (Throwable e) {
            return null;
        }
    }

    /**
     * The app's requested permissions (all, or only those in only) with what the host needs to
     * plan and check a change: granted, flags, runtime (dangerous) or app-op permission (special
     * access, e.g. SYSTEM_ALERT_WINDOW) and its mode.
     */
    static void permState(String pkg, JSONArray only, StringBuilder sb) throws Exception {
        java.util.Set<String> want = null;
        if (only != null) {
            want = new java.util.HashSet<>();
            for (int i = 0; i < only.length(); i++) want.add(only.getString(i));
        }
        PackageInfo pi = packageInfo(pkg, PackageManager.GET_PERMISSIONS);
        Object pmgr = permMgr();
        Map<String, int[]> states = allPermissionStates(pmgr, pkg);
        Object ops = null;
        sb.append("{\"pkg\":"); Agent.str(sb, pkg);
        sb.append(",\"uid\":").append(pi.applicationInfo.uid);
        sb.append(",\"targetSdk\":").append(pi.applicationInfo.targetSdkVersion);
        sb.append(",\"system\":").append(isSystem(pi.applicationInfo));
        sb.append(",\"requested\":[");
        String[] req = pi.requestedPermissions == null ? new String[0] : pi.requestedPermissions;
        boolean first = true;
        for (int i = 0; i < req.length; i++) {
            String perm = req[i];
            if (want != null && !want.contains(perm)) continue;
            if (!first) sb.append(',');
            first = false;
            boolean granted = pi.requestedPermissionsFlags != null
                && (pi.requestedPermissionsFlags[i] & PackageInfo.REQUESTED_PERMISSION_GRANTED) != 0;
            sb.append("{\"name\":"); Agent.str(sb, perm);
            sb.append(",\"granted\":").append(granted);
            Prot p = protection(pmgr, perm);
            sb.append(",\"protection\":"); Agent.str(sb, p.level);
            if (p.level.equals("dangerous")) {
                int[] st = states == null ? null : states.get(perm);
                sb.append(",\"flags\":");
                flagList(sb, st != null ? st[1] : permFlags(pmgr, pkg, perm));
            }
            if (p.op != null) {
                if (ops == null) ops = appOps();
                int mode = (Integer) method(ops, "checkOperation").invoke(ops, opCode(p.op), pi.applicationInfo.uid, pkg);
                sb.append(",\"op\":"); Agent.str(sb, p.op);
                sb.append(",\"mode\":"); Agent.str(sb, modeName(mode));
            }
            sb.append('}');
        }
        sb.append("]}");
    }

    /**
     * Applies a permission plan, in order. Each step is one of:
     *   {"perm":P,"grant":true|false,"set":[flags],"clear":[flags]}  runtime permission
     *   {"perm":P,"op":"android:...","mode":"allowed|errored|default"} app-op permission
     * Granting and revoking go through UiAutomation (the shell's grant/revoke, as `pm grant`);
     * a revoke kills the app's process, as Settings does. Then the user-set / user-fixed flags.
     */
    static void applyPerms(JSONObject req, StringBuilder sb) throws Exception {
        String pkg = req.getString("pkg");
        JSONArray steps = req.getJSONArray("steps");
        int uid = req.optInt("uid", -1);
        if (uid < 0) uid = packageInfo(pkg, 0).applicationInfo.uid;
        Object pmgr = permMgr();
        JSONArray touched = new JSONArray();
        Object ops = null;
        for (int i = 0; i < steps.length(); i++) {
            JSONObject s = steps.getJSONObject(i);
            String perm = s.getString("perm");
            touched.put(perm);
            if (s.has("op")) {
                if (ops == null) ops = appOps();
                String mode = s.getString("mode");
                int m = -1;
                for (int k = 0; k < MODES.length; k++) if (MODES[k].equals(mode)) m = k;
                if (m < 0) throw new IllegalArgumentException("unknown app-op mode " + mode);
                method(ops, "setMode").invoke(ops, opCode(s.getString("op")), uid, pkg, m);
                continue;
            }
            boolean grant = s.getBoolean("grant");
            if (grant) Agent.ua.grantRuntimePermission(pkg, perm);
            else Agent.ua.revokeRuntimePermission(pkg, perm);
            int set = flagMask(s.optJSONArray("set")), clear = flagMask(s.optJSONArray("clear"));
            updateFlags(pmgr, pkg, perm, set | clear, set);
        }
        permState(pkg, touched, sb); // the state after the steps, which the host checks
    }

    // ---------------------------------------------------------------- open URL

    /**
     * Opens a URL as a link tapped in another app does: ACTION_VIEW, CATEGORY_BROWSABLE,
     * FLAG_ACTIVITY_NEW_TASK, no package, with startActivityAndWait (as `am start -W`). Returns once
     * the target activity (or the app chooser) is displayed.
     */
    static void openURL(String url, StringBuilder sb) throws Exception {
        Uri uri = Uri.parse(url);
        Intent i = new Intent(Intent.ACTION_VIEW, uri).addCategory(Intent.CATEGORY_BROWSABLE)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        String type = null;
        if ("content".equals(uri.getScheme())) {
            Object am = service("activity", "android.app.IActivityManager");
            try { type = (String) method(am, "getProviderMimeType").invoke(am, uri, USER_SYSTEM); } catch (Throwable ignored) {}
        }
        String prevFg = Agent.foreground();
        long t0 = SystemClock.uptimeMillis();
        Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
        Method sm = method(atm, "startActivityAndWait");
        Object wr = Agent.startAndWait(atm, sm, url, null, SHELL, null, i, type, null, null, 0, 0, null, null, Agent.USER_CURRENT);
        long t1 = SystemClock.uptimeMillis();
        int result = wr.getClass().getField("result").getInt(wr);
        if (result < 0) {
            String why = constName(Class.forName("android.app.ActivityManager"), "START_", result);
            if (why.equals("START_INTENT_NOT_RESOLVED") || why.equals("START_CLASS_NOT_FOUND")) {
                throw new IllegalArgumentException("no app handles " + url + " as a link (ACTION_VIEW with CATEGORY_BROWSABLE)");
            }
            throw new IllegalStateException("opening " + url + " failed: " + why);
        }
        ComponentName who = (ComponentName) wr.getClass().getField("who").get(wr);
        String app = who == null ? "" : who.getPackageName();
        if (app.isEmpty()) app = Agent.foreground();
        Agent.setPendingLaunch(app, prevFg, t1);
        sb.append("{\"app\":"); Agent.str(sb, app);
        sb.append(",\"component\":"); Agent.str(sb, who == null ? "" : who.flattenToShortString());
        sb.append(",\"result\":"); Agent.str(sb, constName(Class.forName("android.app.ActivityManager"), "START_", result));
        sb.append(",\"startMs\":").append(t1 - t0).append('}');
    }

    // ---------------------------------------------------------------- clipboard

    static Object clipboard() throws Exception { return service("clipboard", "android.content.IClipboard"); }

    /** The primary clip's text as the shell reads it ("" when there is none). */
    static String clipText() throws Exception {
        ClipData c = (ClipData) call(clipboard(), "getPrimaryClip", SHELL, new String[]{null}, new int[]{USER_SYSTEM, 0}, 0, false);
        if (c == null || c.getItemCount() == 0) return "";
        CharSequence t = c.getItemAt(0).getText();
        if (t == null && c.getItemAt(0).getHtmlText() != null) t = c.getItemAt(0).getHtmlText();
        return t == null ? "" : t.toString();
    }

    /** Copies text as another app copying it would; "" clears the clipboard. */
    static void setClip(String text) throws Exception {
        Object cb = clipboard();
        if (text.isEmpty()) {
            call(cb, "clearPrimaryClip", SHELL, new String[]{null}, new int[]{USER_SYSTEM, 0}, 0, false);
            return;
        }
        ClipData c = ClipData.newPlainText("agemu", text);
        call(cb, "setPrimaryClip", c, new String[]{SHELL, null}, new int[]{USER_SYSTEM, 0}, 0, false);
    }

    // ---------------------------------------------------------------- night mode

    static final int MODE_NIGHT_NO = 1, MODE_NIGHT_YES = 2;

    /** Whether the top activity handles uiMode changes itself: 1 yes, 0 no, -1 unknown. */
    static int topHandlesUiMode() {
        try {
            Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
            List<?> tasks = (List<?>) call(atm, "getTasks", null, new String[]{}, new int[]{1}, 0, false);
            if (tasks == null || tasks.isEmpty()) return -1;
            ComponentName top = (ComponentName) tasks.get(0).getClass().getField("topActivity").get(tasks.get(0));
            if (top == null) return -1;
            android.content.pm.ActivityInfo ai = (android.content.pm.ActivityInfo) call(pm(), "getActivityInfo", top, new String[]{}, new int[]{USER_SYSTEM}, 0L, false);
            if (ai == null) return -1;
            return (ai.configChanges & android.content.pm.ActivityInfo.CONFIG_UI_MODE) != 0 ? 1 : 0;
        } catch (Throwable e) {
            return -1;
        }
    }

    /** "dark" or "light" as apps see it: the global configuration's night bit. */
    static String appearance() throws Exception {
        Object am = service("activity", "android.app.IActivityManager");
        Configuration c = (Configuration) method(am, "getConfiguration").invoke(am);
        return (c.uiMode & Configuration.UI_MODE_NIGHT_MASK) == Configuration.UI_MODE_NIGHT_YES ? "dark" : "light";
    }

    /**
     * Sets the night mode as `cmd uimode night yes|no` (and the Dark theme switch, in manual mode)
     * do, then waits until the global configuration has it and, bounded by settleMs, until the
     * focused app has redrawn for it.
     */
    static void setAppearance(String mode, long settleMs, long graceMs, StringBuilder sb) throws Exception {
        boolean dark = mode.equals("dark");
        String want = dark ? "dark" : "light";
        long t0 = SystemClock.uptimeMillis();
        boolean changed = !appearance().equals(want);
        int win = Agent.fgWindowId();
        Object um = service("uimode", "android.app.IUiModeManager");
        method(um, "setNightMode").invoke(um, dark ? MODE_NIGHT_YES : MODE_NIGHT_NO);
        while (!appearance().equals(want)) {
            if (SystemClock.uptimeMillis() - t0 > 3000) throw new IllegalStateException("night mode did not change to " + want + " in 3 s");
            SystemClock.sleep(2);
        }
        long t1 = SystemClock.uptimeMillis();
        String settled = "unchanged";
        if (changed && settleMs > 0) {
            // An activity that declares uiMode in configChanges redraws in place (no new window to
            // wait for); any other is recreated, so wait for its new window, up to settleMs.
            int handles = topHandlesUiMode();
            if (handles == 1) settled = "in-place";
            else settled = Agent.awaitWindowReplaced(win, handles == 0 ? settleMs : graceMs, settleMs);
        }
        sb.append("{\"appearance\":"); Agent.str(sb, want);
        sb.append(",\"applyMs\":").append(t1 - t0);
        sb.append(",\"settleMs\":").append(SystemClock.uptimeMillis() - t1);
        sb.append(",\"settled\":"); Agent.str(sb, settled);
        sb.append('}');
    }
}
