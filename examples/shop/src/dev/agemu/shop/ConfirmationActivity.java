package dev.agemu.shop;

import android.app.Activity;
import android.os.Bundle;
import android.widget.LinearLayout;

/** The end of the flow: an order number an agent can read back. */
public class ConfirmationActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        int total = getIntent().getIntExtra("total", 0);
        int items = getIntent().getIntExtra("items", 0);
        String name = getIntent().getStringExtra("name");
        // Deterministic per order contents, so two runs of the same task show the same number.
        int order = 10000 + Math.abs((total * 31 + items * 7 + (name == null ? 0 : name.hashCode())) % 90000);
        LinearLayout root = Ui.screen(this);
        root.addView(Ui.title(this, "Order confirmed"));
        root.addView(Ui.text(this, "Order #" + order, 22, Ui.ACCENT), Ui.wide(this, 16));
        root.addView(Ui.text(this, items + " items · " + Ui.money(total), 18, Ui.INK), Ui.wide(this, 12));
        root.addView(Ui.text(this, "Thank you, " + name + ". We'll e-mail you when it ships.", 17, Ui.MUTED), Ui.wide(this, 12));
    }
}
