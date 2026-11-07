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

/** A product: price, description, a quantity stepper, Add to cart and a link to the cart. */
public class ProductActivity extends Activity {
    int qty = 1;

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        final int idx = getIntent().getIntExtra("product", 0);
        final Store.Product p = Store.PRODUCTS[idx];
        LinearLayout root = Ui.screen(this);

        Button back = Ui.secondary(this, "‹ Catalog");
        back.setOnClickListener(new View.OnClickListener() { public void onClick(View v) { finish(); } });
        root.addView(back, new LinearLayout.LayoutParams(ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT));

        root.addView(Ui.title(this, p.name), Ui.wide(this, 16));
        root.addView(Ui.text(this, p.category, 15, Ui.MUTED));
        root.addView(Ui.text(this, Ui.money(p.cents), 26, Ui.ACCENT), Ui.wide(this, 16));
        root.addView(Ui.text(this, p.blurb, 17, Ui.INK), Ui.wide(this, 12));

        LinearLayout stepper = Ui.row(this);
        stepper.addView(Ui.text(this, "Quantity", 17, Ui.INK), new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
        Button minus = Ui.secondary(this, "−");
        final TextView count = Ui.text(this, "1", 20, Ui.INK);
        count.setGravity(Gravity.CENTER);
        count.setContentDescription("Quantity 1");
        Button plus = Ui.secondary(this, "+");
        minus.setContentDescription("Decrease quantity");
        plus.setContentDescription("Increase quantity");
        int w = Ui.dp(this, 56);
        stepper.addView(minus, new LinearLayout.LayoutParams(w, w));
        stepper.addView(count, new LinearLayout.LayoutParams(w, ViewGroup.LayoutParams.WRAP_CONTENT));
        stepper.addView(plus, new LinearLayout.LayoutParams(w, w));
        View.OnClickListener step = new View.OnClickListener() {
            public void onClick(View v) {
                qty = Math.max(1, Math.min(9, qty + (v.getContentDescription().toString().startsWith("Increase") ? 1 : -1)));
                count.setText(String.valueOf(qty));
                count.setContentDescription("Quantity " + qty);
            }
        };
        minus.setOnClickListener(step);
        plus.setOnClickListener(step);
        root.addView(stepper, Ui.wide(this, 28));

        final TextView added = Ui.text(this, "", 16, Ui.MUTED);
        Button add = Ui.primary(this, "Add to cart");
        add.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                Store.add(idx, qty);
                added.setText("Added " + qty + " × " + p.name + " · cart: " + Store.count());
            }
        });
        root.addView(add, Ui.wide(this, 28));
        root.addView(added, Ui.wide(this, 12));

        Button cart = Ui.secondary(this, "View cart");
        cart.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { startActivity(new Intent(ProductActivity.this, CartActivity.class)); }
        });
        root.addView(cart, Ui.wide(this, 12));
    }
}
