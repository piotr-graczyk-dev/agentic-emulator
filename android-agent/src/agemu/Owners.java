package agemu;

import java.util.List;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

/**
 * Whether the app process that shows a window is gone: dead, or crashing and never going to answer
 * a read of the window (see Windows). Plain Java on purpose: android-agent/test runs these rules on
 * the host (internal/android/agent_rules_test.go).
 *
 * A process is gone when ActivityManager no longer lists it as running (it leaves out a process
 * that is crashing, from the moment the crash reaches it), when it is a zombie, or while
 * crash_dump has it stopped to write a native crash's tombstone (before ActivityManager hears of
 * the crash). A process that is not responding (an ANR) is not gone once ActivityManager lists
 * its NOT_RESPONDING report: reads of its window wait as they always did. But ActivityManager
 * marks the process not responding (which drops it from the running list) before it dumps the
 * ANR's stack traces and only then files the report, so during the dump (about 0.6-0.8 s on
 * agemu-lab) a live, not-responding app counts as gone and a pending read of its window is
 * dropped. Telling that dump apart would need positive evidence of a crash (a CRASHED entry, a
 * zombie or crash_dump in /proc, or the process's pid gone from /proc); not done.
 */
final class Owners {
    /** The process states procState reports. */
    static final int RUNNING = 0, DEAD = 1, DUMPING = 2;

    /** ActivityManager.ProcessErrorStateInfo.NOT_RESPONDING. */
    static final int NOT_RESPONDING = 2;

    /**
     * Who shows a window: its task's top process (name and uid), or, when its task is not known,
     * only the package its content came from the last time it was read (then any process of the
     * package counts, which never takes a live process for a gone one).
     */
    static final class Owner {
        final String process; // null: only the package is known
        final int uid;
        final String pkg;

        private Owner(String process, int uid, String pkg) {
            this.process = process;
            this.uid = uid;
            this.pkg = pkg;
        }

        static Owner process(String name, int uid) { return new Owner(name, uid, null); }

        static Owner pkg(String pkg) { return new Owner(null, -1, pkg); }

        public String toString() { return process != null ? process + "/" + uid : pkg; }
    }

    /** A process as ActivityManager.getRunningAppProcesses lists it. */
    static final class Proc {
        final String name;
        final int uid, pid;
        final String[] pkgs;

        Proc(String name, int uid, int pid, String[] pkgs) {
            this.name = name;
            this.uid = uid;
            this.pid = pid;
            this.pkgs = pkgs == null ? new String[0] : pkgs;
        }
    }

    /** A process as ActivityManager.getProcessesInErrorState lists it. */
    static final class Err {
        final String name;
        final int uid, condition;

        Err(String name, int uid, int condition) {
            this.name = name;
            this.uid = uid;
            this.condition = condition;
        }
    }

    /** The state of a process by pid (RUNNING, DEAD or DUMPING; see Windows.PROC). */
    interface ProcState {
        int of(int pid);
    }

    /** Whether the target of a pending read is gone (see await). */
    interface Check {
        boolean gone();
    }

    /**
     * The result of read, or null as soon as check says its target is gone; check runs each time
     * the read has not answered for everyMs more. The read itself is left running (it cannot be cut).
     */
    static <T> T await(Future<T> read, long everyMs, Check check) throws Exception {
        while (true) {
            try {
                return read.get(everyMs, TimeUnit.MILLISECONDS);
            } catch (TimeoutException e) {
                if (check.gone()) return null;
            } catch (ExecutionException e) {
                Throwable c = e.getCause();
                if (c instanceof Exception) throw (Exception) c;
                throw new RuntimeException(c);
            }
        }
    }

    /**
     * Whether o's process is gone, given the running processes, the processes in an error state and
     * each pid's state. A null list means it could not be read: then nothing is gone.
     */
    static boolean gone(Owner o, List<Proc> running, List<Err> errors, ProcState st) {
        if (o == null || running == null || errors == null) return false;
        for (Err e : errors) {
            if (e.condition == NOT_RESPONDING && (o.process != null
                    ? o.process.equals(e.name) && o.uid == e.uid
                    : e.name.equals(o.pkg) || e.name.startsWith(o.pkg + ":"))) {
                return false;
            }
        }
        for (Proc p : running) {
            if (owns(o, p) && st.of(p.pid) == RUNNING) return false;
        }
        return true;
    }

    static boolean owns(Owner o, Proc p) {
        if (o.process != null) return o.process.equals(p.name) && o.uid == p.uid;
        for (String k : p.pkgs) if (k.equals(o.pkg)) return true;
        return false;
    }

    /**
     * The state of a process from its /proc/<pid>/stat line and, when it is stopped, the name of
     * the process tracing it (null: none or unknown): a zombie (or dead) process is DEAD, one that
     * crash_dump stopped is DUMPING; anything else, including a process a debugger stopped, RUNNING.
     */
    static int state(String stat, String tracer) {
        if (stat == null) return DEAD;
        int i = stat.lastIndexOf(')'); // the name in parentheses may hold spaces or ')'
        if (i < 0 || i + 2 >= stat.length()) return RUNNING;
        char s = stat.charAt(i + 2);
        if (s == 'Z' || s == 'X' || s == 'x') return DEAD;
        if ((s == 't' || s == 'T') && tracer != null && tracer.startsWith("crash_dump")) return DUMPING;
        return RUNNING;
    }

    /**
     * Whether the front window, whose content belongs to package p, is the window a launch of pkg
     * waits for (Agent.appWindowReady): the app's own window, or a new window (a task's top activity
     * may belong to another package, e.g. Settings' search screen), but neither the app that was in
     * front before (prevFg) nor the home screen (home; "" if unknown), which shows for a moment
     * when the app in front before the launch crashed and its task closed.
     */
    static boolean launchedWindow(String p, String pkg, String prevFg, String home) {
        return p.equals(pkg) || (!p.equals(prevFg) && !p.equals(home));
    }

    /** The TracerPid of a /proc/<pid>/status text (0: none or unknown). */
    static int tracerPid(String status) {
        if (status == null) return 0;
        for (String line : status.split("\n")) {
            if (line.startsWith("TracerPid:")) {
                try {
                    return Integer.parseInt(line.substring("TracerPid:".length()).trim());
                } catch (NumberFormatException e) {
                    return 0;
                }
            }
        }
        return 0;
    }
}
