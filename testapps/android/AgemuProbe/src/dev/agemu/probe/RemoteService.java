package dev.agemu.probe;

import android.app.Service;
import android.content.Intent;
import android.os.IBinder;
import android.os.Process;
import android.util.Log;

/**
 * Runs in dev.agemu.probe:remote, a second process of the app (same uid), so agemu's logs and
 * crashes can be checked for all of an app's processes. Logs one line per start, or throws.
 */
public class RemoteService extends Service {
    @Override
    public int onStartCommand(Intent i, int flags, int startId) {
        int n = i == null ? 0 : i.getIntExtra("n", 0);
        Log.i("AgemuLabRemote", "remote: started #" + n + " pid " + Process.myPid());
        if (i != null && i.getBooleanExtra("crash", false)) {
            throw new IllegalArgumentException("remote crash #" + n);
        }
        return START_NOT_STICKY;
    }

    @Override
    public IBinder onBind(Intent i) { return null; }
}
