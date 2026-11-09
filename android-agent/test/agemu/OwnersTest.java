package agemu;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Callable;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.TimeUnit;

/**
 * Host-side tests of Owners (plain Java, no Android classes). Run by
 * internal/android/agent_rules_test.go: javac + java, exit status 1 on the first failure.
 * The process lists below are what ActivityManager and /proc showed on agemu-lab around crashes of
 * the probe app (dev.agemu.probe, uid 10229).
 */
public final class OwnersTest {
    static int failures;

    static void check(boolean ok, String what) {
        if (!ok) {
            failures++;
            System.out.println("FAIL: " + what);
        }
    }

    static final String PROBE = "dev.agemu.probe";
    static final int UID = 10229;

    static Owners.Proc proc(String name, int uid, int pid, String... pkgs) { return new Owners.Proc(name, uid, pid, pkgs); }

    static Owners.ProcState states(final Map<Integer, Integer> m) {
        return new Owners.ProcState() {
            public int of(int pid) {
                Integer s = m.get(pid);
                return s == null ? Owners.RUNNING : s;
            }
        };
    }

    static final Owners.ProcState ALL_RUNNING = states(new HashMap<Integer, Integer>());

    static List<Owners.Proc> base() {
        List<Owners.Proc> l = new ArrayList<>();
        l.add(proc("system", 1000, 704, "android"));
        l.add(proc("com.android.systemui", 10180, 1065, "com.android.systemui"));
        l.add(proc("com.google.android.apps.nexuslauncher", 10190, 1212, "com.google.android.apps.nexuslauncher"));
        return l;
    }

    static List<Owners.Proc> withProbe(int pid) {
        List<Owners.Proc> l = base();
        l.add(proc(PROBE, UID, pid, PROBE));
        return l;
    }

    static final List<Owners.Err> NO_ERRORS = Collections.emptyList();

    public static void main(String[] args) {
        Owners.Owner byTask = Owners.Owner.process(PROBE, UID);
        Owners.Owner byPkg = Owners.Owner.pkg(PROBE);

        // A live app is never gone, whatever the owner is known by.
        check(!Owners.gone(byTask, withProbe(4301), NO_ERRORS, ALL_RUNNING), "running process (task owner) is gone");
        check(!Owners.gone(byPkg, withProbe(4301), NO_ERRORS, ALL_RUNNING), "running process (package owner) is gone");
        Owners.Owner launcher = Owners.Owner.process("com.google.android.apps.nexuslauncher", 10190);
        check(!Owners.gone(launcher, withProbe(4301), NO_ERRORS, ALL_RUNNING), "the launcher is gone");

        // System windows: "keeps stopping" and other system dialogs are system_server's (content
        // package "android", no task), the status bar SystemUI's; both stay while an app crashes.
        List<Owners.Err> probeCrashed = Arrays.asList(new Owners.Err(PROBE, UID, 1));
        List<Owners.Proc> sys = base();
        sys.set(0, proc("system", 1000, 704, "android", "com.android.providers.settings", "com.android.server.telecom"));
        check(!Owners.gone(Owners.Owner.pkg("android"), sys, probeCrashed, ALL_RUNNING), "a system dialog is gone during a crash");
        check(!Owners.gone(Owners.Owner.pkg("com.android.systemui"), sys, probeCrashed, ALL_RUNNING), "the status bar is gone during a crash");

        // Java crash: ActivityManager drops the process from the running list and lists it as CRASHED.
        List<Owners.Err> crashed = Arrays.asList(new Owners.Err(PROBE, UID, 1));
        check(Owners.gone(byTask, base(), crashed, ALL_RUNNING), "crashing process (task owner) is not gone");
        check(Owners.gone(byPkg, base(), crashed, ALL_RUNNING), "crashing process (package owner) is not gone");
        // ... while the other apps stay live.
        check(!Owners.gone(launcher, base(), crashed, ALL_RUNNING), "the launcher is gone during another app's crash");

        // Exit / killed: no longer listed at all.
        check(Owners.gone(byTask, base(), NO_ERRORS, ALL_RUNNING), "dead process (task owner) is not gone");
        check(Owners.gone(byPkg, base(), NO_ERRORS, ALL_RUNNING), "dead process (package owner) is not gone");

        // Still listed, but a zombie, or stopped by crash_dump (native crash, before ActivityManager knows).
        Map<Integer, Integer> st = new HashMap<>();
        st.put(4301, Owners.DEAD);
        check(Owners.gone(byTask, withProbe(4301), NO_ERRORS, states(st)), "zombie process is not gone");
        st.put(4301, Owners.DUMPING);
        check(Owners.gone(byTask, withProbe(4301), NO_ERRORS, states(st)), "process stopped by crash_dump is not gone");
        check(Owners.gone(byPkg, withProbe(4301), NO_ERRORS, states(st)), "process stopped by crash_dump (package owner) is not gone");

        // ANR: ActivityManager leaves the process out of the running list too, but its window is live.
        List<Owners.Err> anr = Arrays.asList(new Owners.Err(PROBE, UID, Owners.NOT_RESPONDING));
        check(!Owners.gone(byTask, base(), anr, ALL_RUNNING), "not-responding process (task owner) is gone");
        check(!Owners.gone(byPkg, base(), anr, ALL_RUNNING), "not-responding process (package owner) is gone");
        List<Owners.Err> anrRemote = Arrays.asList(new Owners.Err(PROBE + ":remote", UID, Owners.NOT_RESPONDING));
        check(!Owners.gone(byPkg, base(), anrRemote, ALL_RUNNING), "package with a not-responding process is gone");
        // ... but another app's ANR does not keep a crashed one.
        List<Owners.Err> otherAnr = Arrays.asList(new Owners.Err("dev.agemu.probe2", 10230, Owners.NOT_RESPONDING));
        check(Owners.gone(byPkg, base(), otherAnr, ALL_RUNNING), "another package's ANR keeps a dead one");

        // Task owner: the exact process. A crash of the app's :remote process leaves the main one live;
        // a live :remote process does not keep a dead main process.
        List<Owners.Proc> remoteOnly = base();
        remoteOnly.add(proc(PROBE + ":remote", UID, 4400, PROBE));
        check(Owners.gone(byTask, remoteOnly, NO_ERRORS, ALL_RUNNING), "the :remote process keeps a dead main process (task owner)");
        List<Owners.Err> remoteCrashed = Arrays.asList(new Owners.Err(PROBE + ":remote", UID, 1));
        check(!Owners.gone(byTask, withProbe(4301), remoteCrashed, ALL_RUNNING), "a :remote crash makes the main process gone");
        // Package owner: any live process of the package keeps the window (never a wrong skip).
        check(!Owners.gone(byPkg, remoteOnly, NO_ERRORS, ALL_RUNNING), "package with a live :remote process is gone");
        // Same name, another user (uid): not this window's process.
        List<Owners.Proc> otherUser = base();
        otherUser.add(proc(PROBE, 1010229, 5000, PROBE));
        check(Owners.gone(byTask, otherUser, NO_ERRORS, ALL_RUNNING), "another user's process keeps the window");
        // A relaunched app (a new pid, running) is live.
        check(!Owners.gone(byTask, withProbe(4999), NO_ERRORS, ALL_RUNNING), "relaunched process is gone");

        // Lists that could not be read: nothing is gone. No owner: nothing is gone.
        check(!Owners.gone(byTask, null, NO_ERRORS, ALL_RUNNING), "unknown running list makes the window gone");
        check(!Owners.gone(byTask, base(), null, ALL_RUNNING), "unknown error list makes the window gone");
        check(!Owners.gone(null, base(), NO_ERRORS, ALL_RUNNING), "unknown owner is gone");

        // /proc parsing (lines as the device wrote them).
        check(Owners.state("4301 (dev.agemu.probe) S 433 433 0 0 -1 1077952832", null) == Owners.RUNNING, "S is not RUNNING");
        check(Owners.state("4301 (dev.agemu.probe) R 433 433 0", null) == Owners.RUNNING, "R is not RUNNING");
        check(Owners.state("4811 (dev.agemu.probe) Z 433 433 0", null) == Owners.DEAD, "Z is not DEAD");
        check(Owners.state("4811 (dev.agemu.probe) X 433 433 0", null) == Owners.DEAD, "X is not DEAD");
        check(Owners.state(null, null) == Owners.DEAD, "a missing stat is not DEAD");
        check(Owners.state("4688 (dev.agemu.probe) t 433 433 0", "crash_dump64") == Owners.DUMPING, "t by crash_dump64 is not DUMPING");
        check(Owners.state("4688 (dev.agemu.probe) t 433 433 0", "crash_dump32") == Owners.DUMPING, "t by crash_dump32 is not DUMPING");
        check(Owners.state("4688 (dev.agemu.probe) t 433 433 0", "lldb-server") == Owners.RUNNING, "a debugger's stop is not RUNNING");
        check(Owners.state("4688 (dev.agemu.probe) T 433 433 0", null) == Owners.RUNNING, "SIGSTOP is not RUNNING");
        check(Owners.state("77 (a) b) Z 1 1 0", null) == Owners.DEAD, "a name with ') ' is misread");
        check(Owners.state("77 (x)", null) == Owners.RUNNING, "a cut line is not RUNNING");
        String status = "Name:\tdev.agemu.probe\nUmask:\t0077\nState:\tt (tracing stop)\nTgid:\t4688\nNgid:\t0\nPid:\t4688\nPPid:\t433\nTracerPid:\t4743\nUid:\t10229\t10229\t10229\t10229\n";
        check(Owners.tracerPid(status) == 4743, "TracerPid not read");
        check(Owners.tracerPid(status.replace("4743", "0")) == 0, "TracerPid 0 not read");
        check(Owners.tracerPid(null) == 0, "no status gives a tracer");
        check(Owners.tracerPid("Name:\tx\n") == 0, "a status without TracerPid gives a tracer");

        // The window a launch waits for.
        String settings = "com.android.settings", home = "com.google.android.apps.nexuslauncher";
        check(Owners.launchedWindow(settings, settings, PROBE, home), "the launched app's window is not the launch's");
        check(Owners.launchedWindow("com.google.android.settings.intelligence", settings, home, home),
                "another package's activity in the launched task is not the launch's");
        check(!Owners.launchedWindow(home, settings, home, home), "the home screen the launch started from is the launch's");
        check(!Owners.launchedWindow(PROBE, settings, PROBE, home), "the app in front before is the launch's");
        // After a crash: the crashed app's window (its package learned) and the home screen its
        // closing task shows are both not the launch's window.
        check(!Owners.launchedWindow(home, settings, PROBE, home), "the home screen after a crash is the launch's");
        check(Owners.launchedWindow(home, home, PROBE, home), "a launch of home does not end on home");
        check(Owners.launchedWindow(home, settings, PROBE, ""), "an unknown home excludes a window");

        // await on its own thread, bounded: an await that never gives up must fail, not hang.
        Thread t = new Thread(new Runnable() {
            public void run() { testAwait(); }
        });
        t.setDaemon(true);
        t.start();
        try {
            t.join(10000);
        } catch (InterruptedException ignored) {
        }
        check(!t.isAlive(), "await did not return within 10 s");

        if (failures > 0) {
            System.out.println(failures + " failure(s)");
            System.exit(1);
        }
        System.out.println("ok");
        System.exit(0); // the reads left behind below never end
    }

    /** Counts the checks and says gone from the given check on (0: never). */
    static final class Checks implements Owners.Check {
        final int goneAt;
        final CountDownLatch counted; // counted down at each check, or null
        volatile int n;

        Checks(int goneAt) { this(goneAt, null); }

        Checks(int goneAt, CountDownLatch counted) {
            this.goneAt = goneAt;
            this.counted = counted;
        }

        public boolean gone() {
            int k = ++n;
            if (counted != null) counted.countDown();
            return k == goneAt || (goneAt > 0 && k > goneAt);
        }
    }

    static void testAwait() {
        ExecutorService ex = Executors.newCachedThreadPool(new ThreadFactory() {
            public Thread newThread(Runnable r) {
                Thread t = new Thread(r);
                t.setDaemon(true);
                return t;
            }
        });
        final CountDownLatch never = new CountDownLatch(1);
        Callable<String> stuck = new Callable<String>() {
            public String call() throws Exception {
                never.await(); // a read the dying app never answers
                return "late";
            }
        };
        try {
            // An answer within the first interval: no check at all.
            Checks c = new Checks(1);
            String r = Owners.await(ex.submit(new Callable<String>() {
                public String call() { return "root"; }
            }), 200, c);
            check("root".equals(r) && c.n == 0, "a quick read: " + r + " after " + c.n + " checks");

            // A read whose app is gone at the third check: given up then, not before, not at 5 s.
            c = new Checks(3);
            long t0 = System.nanoTime();
            r = Owners.await(ex.submit(stuck), 20, c);
            long ms = (System.nanoTime() - t0) / 1000000;
            check(r == null && c.n == 3, "a stuck read: " + r + " after " + c.n + " checks");
            check(ms >= 55 && ms < 2000, "a stuck read gone at the third check took " + ms + " ms");

            // A slow read of a live app (never gone) is waited for: its answer comes back. It
            // answers after the third check rather than after a fixed time, so a loaded host (CI
            // runs the test packages in parallel) cannot make it answer before three checks.
            final CountDownLatch threeChecks = new CountDownLatch(3);
            c = new Checks(0, threeChecks);
            r = Owners.await(ex.submit(new Callable<String>() {
                public String call() throws Exception {
                    return threeChecks.await(10, TimeUnit.SECONDS) ? "slow" : "no checks in 10 s";
                }
            }), 20, c);
            check("slow".equals(r) && c.n >= 3, "a slow live read: " + r + " after " + c.n + " checks");

            // The read's own failure reaches the caller as it is.
            try {
                Owners.await(ex.submit(new Callable<String>() {
                    public String call() throws Exception { throw new IllegalStateException("no window"); }
                }), 20, new Checks(0));
                check(false, "a failed read returned");
            } catch (IllegalStateException e) {
                check("no window".equals(e.getMessage()), "a failed read: " + e);
            }
        } catch (Exception e) {
            check(false, "await: " + e);
        }
    }
}
