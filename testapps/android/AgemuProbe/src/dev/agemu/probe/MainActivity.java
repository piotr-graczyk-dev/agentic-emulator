package dev.agemu.probe;

import android.app.Activity;
import android.app.NotificationManager;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.content.res.Configuration;
import android.graphics.Color;
import android.location.Location;
import android.location.LocationListener;
import android.location.LocationManager;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.Process;
import android.provider.Settings;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

/**
 * Shows, one line each, what this app observes of the state agemu's test capabilities change:
 * the URL that opened it, its runtime permissions, the GPS fix, the clipboard, the night mode,
 * a value stored in its own data, and its pid. Lines are plain text ("camera: granted") so an
 * agent can wait for them with --wait-contains.
 */
public class MainActivity extends Activity {
    static final String[][] PERMS = {
        {"camera", "android.permission.CAMERA"},
        {"microphone", "android.permission.RECORD_AUDIO"},
        {"fine-location", "android.permission.ACCESS_FINE_LOCATION"},
        {"coarse-location", "android.permission.ACCESS_COARSE_LOCATION"},
        {"background-location", "android.permission.ACCESS_BACKGROUND_LOCATION"},
        {"contacts", "android.permission.READ_CONTACTS"},
        {"calendar", "android.permission.READ_CALENDAR"},
        {"notifications", "android.permission.POST_NOTIFICATIONS"},
        {"photos", "android.permission.READ_MEDIA_IMAGES"},
    };

    final Handler h = new Handler(Looper.getMainLooper());
    LinearLayout box;
    TextView url, perms, loc, clip, mode, store, pid, overlay;
    String lastUrl = "none";
    Location fix;
    LocationManager lm;
    final LocationListener listener = new LocationListener() {
        public void onLocationChanged(Location l) { fix = l; render(); }
        public void onProviderEnabled(String p) {}
        public void onProviderDisabled(String p) {}
        public void onStatusChanged(String p, int s, Bundle b) {}
    };
    boolean listening;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(32, 96, 32, 32);
        url = line(); perms = line(); loc = line(); clip = line(); mode = line(); store = line(); pid = line(); overlay = line();
        Button refresh = new Button(this);
        refresh.setText("Refresh");
        refresh.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { readClipboard(); render(); }
        });
        box.addView(refresh);
        Button ask = new Button(this);
        ask.setText("Ask camera");
        ask.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { askResult = "asking"; render(); requestPermissions(new String[]{"android.permission.CAMERA"}, 1); }
        });
        box.addView(ask);
        ScrollView sv = new ScrollView(this);
        sv.addView(box);
        setContentView(sv);
        SharedPreferences p = getSharedPreferences("probe", MODE_PRIVATE);
        int n = p.getInt("launches", 0) + 1;
        p.edit().putInt("launches", n).commit();
        ((ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE)).addPrimaryClipChangedListener(
            new ClipboardManager.OnPrimaryClipChangedListener() {
                public void onPrimaryClipChanged() { readClipboard(); render(); }
            });
        lm = (LocationManager) getSystemService(Context.LOCATION_SERVICE);
        take(getIntent());
    }

    TextView line() {
        TextView t = new TextView(this);
        t.setTextSize(18);
        t.setPadding(0, 8, 0, 8);
        box.addView(t);
        return t;
    }

    @Override
    protected void onNewIntent(Intent i) {
        super.onNewIntent(i);
        setIntent(i);
        take(i);
    }

    void take(Intent i) {
        Uri u = i == null ? null : i.getData();
        if (u != null) {
            lastUrl = u.toString();
            boolean browsable = i.hasCategory(Intent.CATEGORY_BROWSABLE);
            lastUrl += browsable ? " (browsable)" : " (not browsable)";
        }
        render();
    }

    @Override
    protected void onResume() {
        super.onResume();
        startLocation();
        render();
        // Re-render while in front: permission and location changes have no callback here.
        h.removeCallbacks(tick);
        h.postDelayed(tick, 500);
    }

    @Override
    protected void onPause() {
        super.onPause();
        h.removeCallbacks(tick);
        if (listening) { lm.removeUpdates(listener); listening = false; }
    }

    final Runnable tick = new Runnable() {
        public void run() { startLocation(); render(); h.postDelayed(this, 500); }
    };

    String clipText = "(not read)";
    String askResult = "-";

    @Override
    public void onRequestPermissionsResult(int code, String[] perms, int[] results) {
        askResult = results.length > 0 && results[0] == PackageManager.PERMISSION_GRANTED ? "granted" : "denied";
        render();
    }

    @Override
    public void onWindowFocusChanged(boolean focus) {
        super.onWindowFocusChanged(focus);
        if (focus) { readClipboard(); render(); }
    }

    void readClipboard() {
        ClipboardManager cm = (ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        ClipData c = cm.getPrimaryClip();
        if (c == null || c.getItemCount() == 0) { clipText = "(empty)"; return; }
        CharSequence t = c.getItemAt(0).getText();
        clipText = t == null ? "(no text)" : "\"" + t + "\"";
    }

    void startLocation() {
        if (listening || checkSelfPermission("android.permission.ACCESS_FINE_LOCATION") != PackageManager.PERMISSION_GRANTED) return;
        try {
            lm.requestLocationUpdates(LocationManager.GPS_PROVIDER, 0, 0f, listener, Looper.getMainLooper());
            listening = true;
            Location l = lm.getLastKnownLocation(LocationManager.GPS_PROVIDER);
            if (l != null && fix == null) fix = l;
        } catch (SecurityException e) {
            fix = null;
        }
    }

    void render() {
        set(url, "url: " + lastUrl);
        StringBuilder sb = new StringBuilder();
        for (String[] p : PERMS) {
            boolean g = checkSelfPermission(p[1]) == PackageManager.PERMISSION_GRANTED;
            sb.append(p[0]).append(": ").append(g ? "granted" : "denied");
            if (!g && shouldShowRequestPermissionRationale(p[1])) sb.append(" (rationale)");
            sb.append('\n');
        }
        NotificationManager nm = (NotificationManager) getSystemService(Context.NOTIFICATION_SERVICE);
        sb.append("notifications-enabled: ").append(nm.areNotificationsEnabled());
        set(perms, sb.toString());
        if (checkSelfPermission("android.permission.ACCESS_FINE_LOCATION") != PackageManager.PERMISSION_GRANTED) {
            set(loc, "location: no permission");
        } else if (fix == null) {
            set(loc, "location: waiting");
        } else {
            set(loc, String.format(java.util.Locale.US, "location: %.5f,%.5f", fix.getLatitude(), fix.getLongitude()));
        }
        set(clip, "clipboard: " + clipText);
        int night = getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
        boolean dark = night == Configuration.UI_MODE_NIGHT_YES;
        set(mode, "appearance: " + (dark ? "dark" : "light"));
        box.setBackgroundColor(dark ? Color.rgb(20, 20, 20) : Color.WHITE);
        int fg = dark ? Color.rgb(235, 235, 235) : Color.BLACK;
        for (TextView t : new TextView[]{url, perms, loc, clip, mode, store, pid, overlay}) t.setTextColor(fg);
        set(store, "launches: " + getSharedPreferences("probe", MODE_PRIVATE).getInt("launches", 0));
        set(pid, "pid: " + Process.myPid());
        set(overlay, "overlay: " + Settings.canDrawOverlays(this) + "\nask camera: " + askResult);
    }

    static void set(TextView t, String s) {
        if (!s.contentEquals(t.getText())) t.setText(s);
    }
}
