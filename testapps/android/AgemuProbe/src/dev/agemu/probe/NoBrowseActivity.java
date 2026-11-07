package dev.agemu.probe;

import android.app.Activity;
import android.os.Bundle;
import android.widget.TextView;

/** Handles agemunb:// without CATEGORY_BROWSABLE: a link must not open it. */
public class NoBrowseActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        TextView t = new TextView(this);
        t.setTextSize(20);
        t.setPadding(32, 96, 32, 32);
        t.setText("nobrowse opened: " + getIntent().getData());
        setContentView(t);
    }
}
