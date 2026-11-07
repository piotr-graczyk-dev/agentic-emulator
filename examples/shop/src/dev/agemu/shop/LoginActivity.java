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

/** Sign in: e-mail and password, a 700 ms simulated request, then the catalog. */
public class LoginActivity extends Activity {
    static final String EMAIL = "test@agemu.dev";
    static final String PASSWORD = "hunter2";

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        LinearLayout root = Ui.screen(this);
        root.addView(Ui.title(this, "Agemu Shop"));
        root.addView(Ui.text(this, "Sign in to continue", 17, Ui.MUTED));

        final EditText email = Ui.field(this, "E-mail", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS);
        final EditText password = Ui.field(this, "Password", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD);
        root.addView(email, Ui.wide(this, 32));
        root.addView(password, Ui.wide(this, 12));

        final TextView status = Ui.text(this, "", 15, Ui.ACCENT);
        root.addView(status, Ui.wide(this, 12));

        final Button signIn = Ui.primary(this, "Sign in");
        root.addView(signIn, Ui.wide(this, 12));
        signIn.setOnClickListener(new View.OnClickListener() {
            public void onClick(View v) {
                final String e = email.getText().toString().trim();
                final String p = password.getText().toString();
                if (!EMAIL.equals(e) || !PASSWORD.equals(p)) {
                    status.setText("Wrong e-mail or password");
                    return;
                }
                signIn.setEnabled(false);
                status.setText("Signing in…");
                new Handler(Looper.getMainLooper()).postDelayed(new Runnable() {
                    public void run() {
                        Store.user = e;
                        startActivity(new Intent(LoginActivity.this, CatalogActivity.class));
                        finish();
                    }
                }, 700);
            }
        });
        root.addView(Ui.text(this, "Demo account: " + EMAIL + " / " + PASSWORD, 14, Ui.MUTED), Ui.wide(this, 24));
    }
}
