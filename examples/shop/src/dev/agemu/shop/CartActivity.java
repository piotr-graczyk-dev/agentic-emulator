package dev.agemu.shop;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.TextView;

import java.util.Map;

/** The cart: one line per product, the total, Checkout. */
public class CartActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout root = Ui.screen(this);
        Button back = Ui.secondary(this, "‹ Back");
        back.setOnClickListener(new View.OnClickListener() { public void onClick(View v) { finish(); } });
        root.addView(back, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT));
        root.addView(Ui.title(this, "Your cart"), Ui.wide(this, 16));

        if (Store.cart.isEmpty()) {
            root.addView(Ui.text(this, "Your cart is empty", 17, Ui.MUTED), Ui.wide(this, 12));
            return;
        }
        for (Map.Entry<Integer, Integer> e : Store.cart.entrySet()) {
            Store.Product p = Store.PRODUCTS[e.getKey()];
            LinearLayout line = Ui.row(this);
            line.setBackground(Ui.rounded(Ui.CARD, Ui.LINE, 14, this));
            int pad = Ui.dp(this, 16);
            line.setPadding(pad, pad, pad, pad);
            line.addView(Ui.text(this, e.getValue() + " × " + p.name, 17, Ui.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
            TextView sum = Ui.text(this, Ui.money(p.cents * e.getValue()), 17, Ui.INK);
            sum.setGravity(Gravity.END);
            line.addView(sum);
            root.addView(line, Ui.wide(this, 10));
        }
        LinearLayout total = Ui.row(this);
        total.addView(Ui.text(this, "Total", 20, Ui.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
        total.addView(Ui.text(this, "Total " + Ui.money(Store.totalCents()), 20, Ui.ACCENT));
        root.addView(total, Ui.wide(this, 20));

        Button checkout = Ui.primary(this, "Checkout");
        checkout.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { startActivity(new Intent(CartActivity.this, CheckoutActivity.class)); }
        });
        root.addView(checkout, Ui.wide(this, 24));
    }
}
