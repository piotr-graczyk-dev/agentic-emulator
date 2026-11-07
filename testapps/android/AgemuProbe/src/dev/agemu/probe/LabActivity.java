package dev.agemu.probe;

import android.app.Activity;
import android.content.Intent;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.Process;
import android.os.SystemClock;
import android.util.Log;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.TextView;

/**
 * Logs and crashes on demand, for agemu's logs, crashes and crash notice (docs/CAPABILITIES.md,
 * "Logs and crashes"). Each button (or the deep link agemulab://&lt;action&gt;, e.g.
 * agemulab://burst?n=5000) first logs "lab: &lt;action&gt; at &lt;currentTimeMillis&gt;" on the tag
 * AgemuLab, then:
 *
 * <ul>
 *   <li>Log levels: one line at each level (V D I W E, and Log.wtf), "level &lt;letter&gt; #&lt;n&gt;";
 *   <li>Crash Java: throws IllegalStateException("probe crash #&lt;n&gt;") from crashJava;
 *   <li>Crash native / Abort native: a null write (SIGSEGV) or abort() with a message, in
 *       libprobe.so (built when the NDK is there; without it the button says so);
 *   <li>ANR: blocks the main thread for 30 s (the next input then times out);
 *   <li>Exit: System.exit(0) without a crash;
 *   <li>Remote log / Crash remote: the :remote process logs a line, or throws;
 *   <li>Log burst: n lines (default 2000) "burst &lt;run&gt; &lt;i&gt;" as fast as Log.i takes them,
 *       then "burst-done run=&lt;run&gt; n=&lt;n&gt; ms=&lt;elapsed&gt;", also shown on screen;
 *   <li>agemulab://spam?n=&lt;lines per second&gt;&amp;s=&lt;seconds&gt; (link only): a background
 *       thread logs "spam &lt;run&gt; &lt;i&gt;" at that rate (default 500/s for 60 s), then
 *       "spam-done run=&lt;run&gt; n=&lt;lines&gt;".
 * </ul>
 */
public class LabActivity extends Activity {
    static final String TAG = "AgemuLab";
    static int count;
    static boolean nativeLoaded;
    static String nativeError = "";

    static {
        try {
            System.loadLibrary("probe");
            nativeLoaded = true;
        } catch (Throwable t) {
            nativeError = t.toString();
        }
    }

    static native void crashNative();

    static native void abortNative(String msg);

    TextView status;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(32, 96, 32, 32);
        status = new TextView(this);
        status.setTextSize(18);
        status.setText("lab: ready pid " + Process.myPid());
        box.addView(status);
        for (final String a : new String[]{"Log levels", "Crash Java", "Crash native", "Abort native", "ANR", "Exit",
                "Remote log", "Crash remote", "Log burst"}) {
            Button bt = new Button(this);
            bt.setText(a);
            bt.setAllCaps(false);
            bt.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) { act(a.toLowerCase().replace(' ', '-'), 0); }
            });
            box.addView(bt);
        }
        setContentView(box);
        Log.i(TAG, "lab: created pid " + Process.myPid());
        take(getIntent());
    }

    @Override
    protected void onNewIntent(Intent i) {
        super.onNewIntent(i);
        take(i);
    }

    void take(Intent i) {
        Uri u = i == null ? null : i.getData();
        if (u == null || !"agemulab".equals(u.getScheme())) return;
        final String action = u.getHost();
        final int fn = intParam(u, "n");
        final int fs = intParam(u, "s");
        // After the activity is shown, so a deep link's launch completes first.
        new Handler(Looper.getMainLooper()).post(new Runnable() {
            public void run() { act(action, fn, fs); }
        });
    }

    static int intParam(Uri u, String name) {
        try {
            String q = u.getQueryParameter(name);
            return q == null ? 0 : Integer.parseInt(q);
        } catch (RuntimeException e) {
            return 0;
        }
    }

    void act(String action, int n) { act(action, n, 0); }

    void act(String action, int n, int secs) {
        count++;
        Log.i(TAG, "lab: " + action + " at " + System.currentTimeMillis());
        switch (action) {
            case "log-levels":
                Log.v(TAG, "level V #" + count);
                Log.d(TAG, "level D #" + count);
                Log.i(TAG, "level I #" + count);
                Log.w(TAG, "level W #" + count);
                Log.e(TAG, "level E #" + count);
                Log.wtf(TAG, "level F #" + count);
                status.setText("lab: logged levels #" + count);
                break;
            case "crash-java":
                crashJava();
                break;
            case "crash-native":
                if (!nativeLoaded) { status.setText("lab: no native library: " + nativeError); break; }
                crashNative();
                break;
            case "abort-native":
                if (!nativeLoaded) { status.setText("lab: no native library: " + nativeError); break; }
                abortNative("probe abort #" + count);
                break;
            case "anr":
                status.setText("lab: blocking the main thread");
                SystemClock.sleep(30000);
                status.setText("lab: unblocked");
                break;
            case "exit":
                System.exit(0);
                break;
            case "remote-log":
            case "crash-remote":
                startService(new Intent(this, RemoteService.class).putExtra("crash", action.equals("crash-remote")).putExtra("n", count));
                status.setText("lab: sent " + action + " #" + count);
                break;
            case "log-burst":
            case "burst":
                burst(n > 0 ? n : 2000);
                break;
            case "spam":
                spam(n > 0 ? n : 500, secs > 0 ? secs : 60);
                break;
            default:
                Log.w(TAG, "lab: unknown action " + action);
        }
    }

    void crashJava() {
        throw new IllegalStateException("probe crash #" + count);
    }

    void burst(int n) {
        int run = count;
        long t0 = System.nanoTime();
        for (int i = 0; i < n; i++) {
            Log.i(TAG, "burst " + run + " " + i);
        }
        long us = (System.nanoTime() - t0) / 1000;
        String done = "burst-done run=" + run + " n=" + n + " ms=" + (us / 1000) + "." + String.format(java.util.Locale.US, "%03d", us % 1000);
        Log.i(TAG, done);
        status.setText("lab: " + done);
    }

    void spam(final int perSecond, final int secs) {
        final int run = count;
        status.setText("lab: spam run " + run + " " + perSecond + "/s for " + secs + " s");
        new Thread("spam") {
            public void run() {
                long start = SystemClock.uptimeMillis();
                long total = (long) perSecond * secs;
                for (long i = 0; i < total; i++) {
                    // Paced against the start, so the rate holds however long Log.i takes.
                    long due = start + i * 1000 / perSecond;
                    long now = SystemClock.uptimeMillis();
                    if (due > now) SystemClock.sleep(due - now);
                    Log.i(TAG, "spam " + run + " " + i);
                }
                Log.i(TAG, "spam-done run=" + run + " n=" + total);
            }
        }.start();
    }
}
