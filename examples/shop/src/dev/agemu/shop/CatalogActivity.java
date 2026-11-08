package dev.agemu.shop;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.Editable;
import android.text.TextWatcher;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

/**
 * The catalog: 36 products in a scrolling list (the last ones are off screen on a phone), a
 * search field that filters after a 300 ms debounce, and a cart button with the item count.
 */
public class CatalogActivity extends Activity {
    LinearLayout list;
    TextView count;
    Button cart;
    final Handler h = new Handler(Looper.getMainLooper());
    String query = "";
    final Runnable filter = new Runnable() { public void run() { render(); } };

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout root = Ui.screen(this);

        LinearLayout top = Ui.row(this);
        TextView t = Ui.title(this, "Catalog");
        top.addView(t, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
        cart = Ui.secondary(this, "Cart (0)");
        cart.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) { startActivity(new Intent(CatalogActivity.this, CartActivity.class)); }
        });
        top.addView(cart);
        root.addView(top);

        EditText search = Ui.field(this, "Search products", 0);
        search.addTextChangedListener(new TextWatcher() {
            public void beforeTextChanged(CharSequence s, int a, int c, int d) {}
            public void onTextChanged(CharSequence s, int a, int c, int d) {}
            public void afterTextChanged(Editable s) {
                query = s.toString().trim().toLowerCase();
                h.removeCallbacks(filter);
                h.postDelayed(filter, 300);
            }
        });
        root.addView(search, Ui.wide(this, 12));
        count = Ui.text(this, "", 14, Ui.MUTED);
        root.addView(count, Ui.wide(this, 8));

        ScrollView sv = new ScrollView(this);
        list = new LinearLayout(this);
        list.setOrientation(LinearLayout.VERTICAL);
        sv.addView(list);
        LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(ViewGroup.LayoutParams.MATCH_PARENT, 0, 1);
        lp.topMargin = Ui.dp(this, 8);
        root.addView(sv, lp);
        render();
    }

    @Override
    protected void onResume() {
        super.onResume();
        cart.setText("Cart (" + Store.count() + ")");
    }

    void render() {
        list.removeAllViews();
        int shown = 0;
        for (int i = 0; i < Store.PRODUCTS.length; i++) {
            final Store.Product p = Store.PRODUCTS[i];
            if (!query.isEmpty() && !p.name.toLowerCase().contains(query) && !p.category.toLowerCase().contains(query)) continue;
            shown++;
            final int idx = i;
            LinearLayout card = Ui.row(this);
            card.setBackground(Ui.rounded(Ui.CARD, Ui.LINE, 14, this));
            int pad = Ui.dp(this, 16);
            card.setPadding(pad, pad, pad, pad);
            LinearLayout col = new LinearLayout(this);
            col.setOrientation(LinearLayout.VERTICAL);
            col.addView(Ui.text(this, p.name, 18, Ui.INK));
            col.addView(Ui.text(this, p.category, 14, Ui.MUTED));
            card.addView(col, new LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1));
            TextView price = Ui.text(this, Ui.money(p.cents), 17, Ui.INK);
            price.setGravity(Gravity.END);
            card.addView(price);
            card.setClickable(true);
            card.setOnClickListener(new View.OnClickListener() {
                public void onClick(View v) {
                    startActivity(new Intent(CatalogActivity.this, ProductActivity.class).putExtra("product", idx));
                }
            });
            list.addView(card, Ui.wide(this, shown == 1 ? 0 : 10));
        }
        count.setText(shown == 1 ? "1 product" : shown + " products");
        if (shown == 0) list.addView(Ui.text(this, "No products match", 16, Ui.MUTED));
    }
}
