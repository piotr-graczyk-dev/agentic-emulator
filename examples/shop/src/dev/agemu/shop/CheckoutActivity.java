package dev.agemu.shop;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.text.InputType;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.TextView;

/** Shipping details, Place order, a 900 ms simulated payment, then the confirmation. */
public class CheckoutActivity extends Activity {
    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout root = Ui.screen(this);
        root.addView(Ui.title(this, "Checkout"));
        root.addView(Ui.text(this, "Total " + Ui.money(Store.totalCents()), 17, Ui.MUTED));

        final EditText name = Ui.field(this, "Full name", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PERSON_NAME);
        final EditText street = Ui.field(this, "Street address", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_POSTAL_ADDRESS);
        final EditText city = Ui.field(this, "City", 0);
        final EditText zip = Ui.field(this, "Postal code", 0);
        root.addView(name, Ui.wide(this, 24));
        root.addView(street, Ui.wide(this, 12));
        root.addView(city, Ui.wide(this, 12));
        root.addView(zip, Ui.wide(this, 12));

        final TextView status = Ui.text(this, "", 15, Ui.ACCENT);
        root.addView(status, Ui.wide(this, 12));
        final Button place = Ui.primary(this, "Place order");
        root.addView(place, Ui.wide(this, 12));
        place.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                for (EditText e : new EditText[]{name, street, city, zip}) {
                    if (e.getText().toString().trim().isEmpty()) {
                        status.setText("Fill in " + e.getHint());
                        return;
                    }
                }
                place.setEnabled(false);
                status.setText("Placing order…");
                final String who = name.getText().toString().trim();
                new Handler(Looper.getMainLooper()).postDelayed(new Runnable() {
                    public void run() {
                        int total = Store.totalCents();
                        int items = Store.count();
                        Store.cart.clear();
                        startActivity(new Intent(CheckoutActivity.this, ConfirmationActivity.class)
                            .putExtra("total", total).putExtra("items", items).putExtra("name", who)
                            .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP));
                        finish();
                    }
                }, 900);
            }
        });
    }
}
