package dev.agemu.shop;

import android.app.Activity;
import android.content.Context;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.text.InputType;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;

/** Views built in code (no resources), one look for every screen. */
final class Ui {
    static final int BG = Color.rgb(0xF6, 0xF4, 0xEF);
    static final int INK = Color.rgb(0x1D, 0x1B, 0x18);
    static final int MUTED = Color.rgb(0x6F, 0x6A, 0x61);
    static final int ACCENT = Color.rgb(0xD9, 0x4F, 0x2B);
    static final int CARD = Color.WHITE;
    static final int LINE = Color.rgb(0xE4, 0xE0, 0xD7);

    private Ui() {}

    static int dp(Context c, float v) {
        return Math.round(TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v, c.getResources().getDisplayMetrics()));
    }

    /** A vertical root that keeps clear of the status bar, navigation bar and keyboard. */
    static LinearLayout screen(final Activity a) {
        final LinearLayout root = new LinearLayout(a);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);
        final int pad = dp(a, 20);
        root.setOnApplyWindowInsetsListener(new View.OnApplyWindowInsetsListener() {
            public WindowInsets onApplyWindowInsets(View v, WindowInsets in) {
                android.graphics.Insets i = in.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.ime());
                v.setPadding(pad, i.top + dp(a, 8), pad, i.bottom + dp(a, 8));
                return WindowInsets.CONSUMED;
            }
        });
        a.setContentView(root);
        // Dark status and navigation bar icons on the light background.
        android.view.WindowInsetsController wic = a.getWindow().getInsetsController();
        if (wic != null) {
            int light = android.view.WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                | android.view.WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
            wic.setSystemBarsAppearance(light, light);
        }
        return root;
    }

    static TextView title(Context c, String s) {
        TextView t = new TextView(c);
        t.setText(s);
        t.setTextColor(INK);
        t.setTextSize(30);
        t.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        t.setPadding(0, dp(c, 12), 0, dp(c, 4));
        return t;
    }

    static TextView text(Context c, String s, float size, int color) {
        TextView t = new TextView(c);
        t.setText(s);
        t.setTextSize(size);
        t.setTextColor(color);
        return t;
    }

    static GradientDrawable rounded(int fill, int stroke, float radiusDp, Context c) {
        GradientDrawable d = new GradientDrawable();
        d.setColor(fill);
        d.setCornerRadius(dp(c, radiusDp));
        if (stroke != 0) d.setStroke(dp(c, 1), stroke);
        return d;
    }

    static EditText field(Context c, String hint, int inputType) {
        EditText e = new EditText(c);
        e.setHint(hint);
        e.setSingleLine(true);
        e.setInputType(inputType == 0 ? InputType.TYPE_CLASS_TEXT : inputType);
        e.setTypeface(Typeface.DEFAULT); // a password input type switches to monospace otherwise
        e.setTextSize(17);
        e.setTextColor(INK);
        e.setHintTextColor(MUTED);
        e.setBackground(rounded(CARD, LINE, 12, c));
        int p = dp(c, 14);
        e.setPadding(p, p, p, p);
        return e;
    }

    static Button primary(Context c, String s) {
        Button b = new Button(c);
        b.setText(s);
        b.setAllCaps(false);
        b.setTextSize(17);
        b.setTextColor(Color.WHITE);
        b.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        b.setBackground(rounded(ACCENT, 0, 14, c));
        b.setStateListAnimator(null);
        return b;
    }

    static Button secondary(Context c, String s) {
        Button b = new Button(c);
        b.setText(s);
        b.setAllCaps(false);
        b.setTextSize(16);
        b.setTextColor(INK);
        b.setBackground(rounded(CARD, LINE, 14, c));
        b.setStateListAnimator(null);
        return b;
    }

    static LinearLayout.LayoutParams wide(Context c, int topDp) {
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        lp.topMargin = dp(c, topDp);
        return lp;
    }

    static LinearLayout row(Context c) {
        LinearLayout r = new LinearLayout(c);
        r.setOrientation(LinearLayout.HORIZONTAL);
        r.setGravity(Gravity.CENTER_VERTICAL);
        return r;
    }

    static String money(int cents) {
        return String.format(java.util.Locale.US, "$%d.%02d", cents / 100, cents % 100);
    }
}
