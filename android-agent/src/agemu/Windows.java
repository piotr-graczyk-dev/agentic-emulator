package agemu;

import android.app.ActivityManager;
import android.os.IBinder;
import android.view.accessibility.AccessibilityNodeInfo;
import android.view.accessibility.AccessibilityWindowInfo;

import java.io.File;
import java.io.FileInputStream;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collection;
import java.util.List;
import java.util.concurrent.Callable;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.SynchronousQueue;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;

/**
 * Reads of a window's content that do not wait for an app that is dying.
 *
 * A window's content comes from its app: AccessibilityWindowInfo.getRoot() (and every node read
 * past the prefetched ones) sends the request to the app's process and waits up to 5 s for the
 * answer (AccessibilityInteractionClient). A process that is crashing when the request reaches it
 * never answers, so the first tree read after an app crash took 5.0 s on agemu-lab (Java, native
 * and abort crashes and System.exit; a read after the process is dead fails at once), and every
 * tree read queued behind it on uaLock waited too: a read the daemon had given up on still held
 * the next call for 4.7 s.
 *
 * So the content is read on a reader thread, and while the read has not answered, the window's
 * owner is checked every CHECK_MS: when its process is gone (Owners: dead, crashing, or stopped by
 * crash_dump), the read is left behind and the window is reported without content, like a window
 * whose root cannot be read. Each thread has its own AccessibilityInteractionClient (and lock),
 * so a read left behind blocks only its thread until the 5 s pass. A window whose owner cannot be
 * told (no task, never read) is waited for as before. An app that is busy keeps the wait, as does
 * one whose ANR ActivityManager has reported; while Android is still dumping an ANR's stack
 * traces, before that report, the not-responding app is listed neither as running nor in error,
 * so its pending read is dropped then, as for a crashed app (see Owners). A read that answers
 * within CHECK_MS costs only the hand-over to the reader thread.
 */
final class Windows {
    /** How long a read runs before (and between) checks of its window's owner. */
    static final long CHECK_MS = 25;

    /**
     * Reader threads: one when no read is stuck, one more for each read left behind (it returns
     * when its read ends; spare threads end after a minute idle).
     */
    static final ThreadPoolExecutor readers = new ThreadPoolExecutor(1, Integer.MAX_VALUE, 60, TimeUnit.SECONDS,
            new SynchronousQueue<Runnable>(), new ThreadFactory() {
                public Thread newThread(Runnable r) {
                    Thread t = new Thread(r, "agemu-read");
                    t.setDaemon(true);
                    return t;
                }
            });

    /** Package of each window's content when it was last read (window id to package). */
    static final ConcurrentHashMap<Integer, String> pkgOf = new ConcurrentHashMap<>();

    /**
     * Runs read (a read of w's content) on a reader thread and returns its result, or null when w's
     * process is gone before it answered.
     */
    static <T> T read(final AccessibilityWindowInfo w, Callable<T> read) throws Exception {
        final Owners.Owner[] gone = new Owners.Owner[1];
        T r = Owners.await(readers.submit(read), CHECK_MS, new Owners.Check() {
            public boolean gone() { return (gone[0] = ownerGone(w)) != null; }
        });
        if (gone[0] != null) {
            System.out.println("agemu-agent: window " + w.getId() + " (" + w.getTitle() + ") of " + gone[0]
                    + ": not listed as running or not responding (crashing, dead or dumping an ANR),"
                    + " or a zombie or stopped by crash_dump; not waiting for its content");
        }
        return r;
    }

    /** w's root node (null: none, or w's process is gone; see read). */
    static AccessibilityNodeInfo root(final AccessibilityWindowInfo w) throws Exception {
        AccessibilityNodeInfo r = read(w, new Callable<AccessibilityNodeInfo>() {
            public AccessibilityNodeInfo call() { return w.getRoot(); }
        });
        if (r != null) learn(w.getId(), Agent.cs(r.getPackageName()));
        return r;
    }

    /** Records the package of a window's content (the owner of a window whose task is not known). */
    static void learn(int windowId, String pkg) {
        if (!pkg.isEmpty()) pkgOf.put(windowId, pkg);
    }

    /** Forgets the windows that are not in ids (closed windows). */
    static void keepOnly(Collection<Integer> ids) {
        pkgOf.keySet().retainAll(ids);
    }

    /** Whether no process of pkg is live (Owners.gone); false when that cannot be told. */
    static boolean appGone(String pkg) {
        try {
            return Owners.gone(Owners.Owner.pkg(pkg), running(), errors(), PROC);
        } catch (Throwable ignored) {
            return false;
        }
    }

    /** w's owner when its process is gone; null when it is not, or that cannot be told. */
    static Owners.Owner ownerGone(AccessibilityWindowInfo w) {
        try {
            Owners.Owner o = ownerOf(w);
            if (o != null && Owners.gone(o, running(), errors(), PROC)) return o;
        } catch (Throwable ignored) {
        }
        return null;
    }

    // ---------------------------------------------------------------- owners

    static Method getTaskId, getTasks;
    static Field taskIdField, topActivityInfo;

    /**
     * The process that shows w: the top activity's process of w's task (an activity's window, its
     * dialogs and pop-ups), unless w's content came from another package when it was last read (a
     * window below the top activity, such as an app under the permission dialog Android starts in
     * its task); then, and for a window without a task, any process of that package.
     */
    static Owners.Owner ownerOf(AccessibilityWindowInfo w) {
        String pkg = pkgOf.get(w.getId());
        try {
            if (getTaskId == null) getTaskId = AccessibilityWindowInfo.class.getMethod("getTaskId");
            int task = (Integer) getTaskId.invoke(w);
            if (task >= 0) {
                for (Object t : tasks()) {
                    if (taskIdField == null) taskIdField = t.getClass().getField("taskId");
                    if (taskIdField.getInt(t) != task) continue;
                    if (topActivityInfo == null) topActivityInfo = t.getClass().getField("topActivityInfo");
                    android.content.pm.ActivityInfo ai = (android.content.pm.ActivityInfo) topActivityInfo.get(t);
                    if (ai != null && ai.processName != null && ai.applicationInfo != null
                            && (pkg == null || pkg.equals(ai.packageName))) {
                        return Owners.Owner.process(ai.processName, ai.applicationInfo.uid);
                    }
                }
            }
        } catch (Throwable ignored) {
            // a task's top process is unknown here: fall back to the package
        }
        return pkg == null ? null : Owners.Owner.pkg(pkg);
    }

    /** The running tasks (IActivityTaskManager.getTasks; its arguments differ between API levels). */
    static List<?> tasks() throws Exception {
        Object atm = Class.forName("android.app.ActivityTaskManager").getMethod("getService").invoke(null);
        if (getTasks == null) {
            for (Method m : atm.getClass().getMethods()) {
                Class<?>[] p = m.getParameterTypes();
                if (m.getName().equals("getTasks") && p.length > 0 && p[0] == int.class) getTasks = m;
            }
        }
        Class<?>[] p = getTasks.getParameterTypes();
        Object[] args = new Object[p.length];
        args[0] = 100; // maxNum
        for (int i = 1; i < p.length; i++) args[i] = p[i] == boolean.class ? (Object) false : (Object) 0; // display 0
        return (List<?>) getTasks.invoke(atm, args);
    }

    static Object activityManager() throws Exception {
        IBinder b = (IBinder) Class.forName("android.os.ServiceManager").getMethod("getService", String.class).invoke(null, "activity");
        return Class.forName("android.app.IActivityManager$Stub").getMethod("asInterface", IBinder.class).invoke(null, b);
    }

    /** ActivityManager's running app processes (it leaves out crashing and not-responding ones). */
    static List<Owners.Proc> running() throws Exception {
        Object am = activityManager();
        List<?> l = (List<?>) am.getClass().getMethod("getRunningAppProcesses").invoke(am);
        if (l == null) return null;
        List<Owners.Proc> out = new ArrayList<>();
        for (Object o : l) {
            ActivityManager.RunningAppProcessInfo p = (ActivityManager.RunningAppProcessInfo) o;
            out.add(new Owners.Proc(p.processName, p.uid, p.pid, p.pkgList));
        }
        return out;
    }

    /** The processes ActivityManager has in an error state (crashing, not responding). */
    static List<Owners.Err> errors() throws Exception {
        Object am = activityManager();
        List<?> l = (List<?>) am.getClass().getMethod("getProcessesInErrorState").invoke(am);
        List<Owners.Err> out = new ArrayList<>();
        if (l != null) {
            for (Object o : l) {
                ActivityManager.ProcessErrorStateInfo e = (ActivityManager.ProcessErrorStateInfo) o;
                out.add(new Owners.Err(e.processName, e.uid, e.condition));
            }
        }
        return out;
    }

    /** A process's state from /proc (Owners.state). */
    static final Owners.ProcState PROC = new Owners.ProcState() {
        public int of(int pid) {
            String stat = readFile("/proc/" + pid + "/stat");
            if (stat == null && new File("/proc/" + pid).exists()) return Owners.RUNNING; // not readable: cannot tell
            String tracer = null;
            int t = Owners.tracerPid(readFile("/proc/" + pid + "/status"));
            if (t > 0) {
                String comm = readFile("/proc/" + t + "/comm");
                if (comm != null) tracer = comm.trim();
            }
            return Owners.state(stat, tracer);
        }
    };

    static String readFile(String path) {
        try (FileInputStream in = new FileInputStream(path)) {
            byte[] buf = new byte[4096];
            int n, off = 0;
            while (off < buf.length && (n = in.read(buf, off, buf.length - off)) > 0) off += n;
            return new String(buf, 0, off, StandardCharsets.UTF_8);
        } catch (Throwable e) {
            return null;
        }
    }
}
