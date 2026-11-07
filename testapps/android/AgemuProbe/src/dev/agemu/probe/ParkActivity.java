package dev.agemu.probe;

import android.app.Activity;
import android.app.AlarmManager;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.content.Intent;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.TextView;
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.InetSocketAddress;
import java.net.Socket;

/**
 * What an app sees of `agemu park` and the resume after it, one plain-text line each, so
 * `agemu ui` reads them:
 *
 * <ul>
 *   <li>park-count: a counter (the Increment button), state that must survive;
 *   <li>park-ticks / park-gap: a 0.5 s Handler ticker and its largest gap between two ticks by
 *       the wall clock, elapsedRealtime and uptimeMillis (a park shows as a gap);
 *   <li>park-net: a TCP connection to the host (10.0.2.2:&lt;port&gt;, the intent extra "port")
 *       that reads lines a host server writes once a second: its state, lines read, reconnects;
 *   <li>park-alarm: an exact alarm set with the Notify button, "notifyS" seconds (default 20)
 *       ahead on the elapsedRealtime clock; when it fires it posts a notification and shows how
 *       late it came by both clocks.
 * </ul>
 *
 * Started with: am start -n dev.agemu.probe/.ParkActivity --ei port 47311 --ei notifyS 20.
 */
public class ParkActivity extends Activity {
    final Handler h = new Handler(Looper.getMainLooper());
    TextView count, ticks, gap, net, alarm;
    int n, tickCount, lines, reconnects;
    long lastWall, lastElapsed, lastUptime, maxWall, maxElapsed, maxUptime;
    String netState = "off", alarmState = "none";
    int port, notifyS;
    volatile boolean stopped;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        Intent in = getIntent();
        port = in.getIntExtra("port", 0);
        notifyS = in.getIntExtra("notifyS", 20);
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(32, 96, 32, 32);
        TextView title = line(box);
        title.setText("Park");
        count = line(box);
        ticks = line(box);
        gap = line(box);
        net = line(box);
        alarm = line(box);
        Button inc = new Button(this);
        inc.setText("Increment");
        inc.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { n++; render(); }
        });
        box.addView(inc);
        Button notify = new Button(this);
        notify.setText("Notify");
        notify.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { schedule(); }
        });
        box.addView(notify);
        setContentView(box);
        lastWall = System.currentTimeMillis();
        lastElapsed = SystemClock.elapsedRealtime();
        lastUptime = SystemClock.uptimeMillis();
        h.postDelayed(tick, 500);
        if (port > 0) {
            new Thread(reader, "park-net").start();
        }
        render();
    }

    @Override
    protected void onDestroy() {
        stopped = true;
        super.onDestroy();
    }

    TextView line(LinearLayout box) {
        TextView t = new TextView(this);
        t.setTextSize(16);
        box.addView(t);
        return t;
    }

    final Runnable tick = new Runnable() {
        public void run() {
            long w = System.currentTimeMillis(), e = SystemClock.elapsedRealtime(), u = SystemClock.uptimeMillis();
            maxWall = Math.max(maxWall, w - lastWall);
            maxElapsed = Math.max(maxElapsed, e - lastElapsed);
            maxUptime = Math.max(maxUptime, u - lastUptime);
            lastWall = w;
            lastElapsed = e;
            lastUptime = u;
            tickCount++;
            render();
            h.postDelayed(this, 500);
        }
    };

    final Runnable reader = new Runnable() {
        public void run() {
            while (!stopped) {
                Socket s = new Socket();
                try {
                    s.connect(new InetSocketAddress("10.0.2.2", port), 5000);
                    setNet("ready");
                    BufferedReader r = new BufferedReader(new InputStreamReader(s.getInputStream(), "UTF-8"));
                    String l;
                    while ((l = r.readLine()) != null) {
                        h.post(new Runnable() {
                            public void run() { lines++; render(); }
                        });
                    }
                    setNet("eof");
                } catch (Exception e) {
                    setNet("error " + e.getClass().getSimpleName());
                } finally {
                    try { s.close(); } catch (Exception ignored) { }
                }
                h.post(new Runnable() {
                    public void run() { reconnects++; render(); }
                });
                SystemClock.sleep(1000);
            }
        }
    };

    void setNet(final String st) {
        h.post(new Runnable() {
            public void run() { netState = st; render(); }
        });
    }

    void schedule() {
        final long dueElapsed = SystemClock.elapsedRealtime() + notifyS * 1000L;
        final long dueWall = System.currentTimeMillis() + notifyS * 1000L;
        AlarmManager am = getSystemService(AlarmManager.class);
        try {
            am.setExact(AlarmManager.ELAPSED_REALTIME_WAKEUP, dueElapsed, "park", new AlarmManager.OnAlarmListener() {
                public void onAlarm() {
                    long late = SystemClock.elapsedRealtime() - dueElapsed;
                    long lateWall = System.currentTimeMillis() - dueWall;
                    alarmState = "fired late=" + late + " wall=" + lateWall;
                    post();
                    render();
                }
            }, h);
            alarmState = "scheduled " + notifyS + " s";
        } catch (SecurityException e) {
            alarmState = "denied " + e.getMessage();
        }
        render();
    }

    void post() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel("park", "Park", NotificationManager.IMPORTANCE_DEFAULT));
        try {
            nm.notify(1, new Notification.Builder(this, "park").setSmallIcon(android.R.drawable.ic_dialog_info)
                    .setContentTitle("park").setContentText(alarmState).build());
        } catch (SecurityException ignored) {
            // POST_NOTIFICATIONS not granted: the alarm line still shows.
        }
    }

    void render() {
        count.setText("park-count: " + n);
        ticks.setText("park-ticks: " + tickCount);
        gap.setText("park-gap: wall=" + maxWall + " elapsed=" + maxElapsed + " uptime=" + maxUptime);
        net.setText("park-net: " + netState + " lines=" + lines + " reconnects=" + reconnects);
        alarm.setText("park-alarm: " + alarmState);
    }
}
