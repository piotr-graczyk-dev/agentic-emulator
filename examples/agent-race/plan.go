package main

import (
	"fmt"
	"strings"
)

// Cond is what an agent waits for after an action. Zero: nothing.
type Cond struct {
	Text     string // an element with exactly this text or label is on screen
	Contains string // an element whose text or label contains this
	Gone     string // no element with exactly this text or label is on screen
}

func (c Cond) zero() bool { return c == Cond{} }

// Driver is the tool set of one arm. Each driver does a step the way that tool is meant to be
// used: Fill is one agent-device `fill`, a tap and a `type` with agemu or adb, a tap and a `fill`
// with Argent; ScrollUntil is one `scroll --until` with agent-device and a swipe-and-look loop
// with the others.
type Driver interface {
	Launch(wait Cond) error
	Tap(target string, wait Cond) error       // the element with this text or label
	Fill(field, text string, wait Cond) error // the text field with this label
	ScrollUntil(target string) error          // swipe up until the element is on screen
	Read(contains string) (string, error)     // the text of an element on screen
	Screenshot(name string) error
}

type step struct {
	label string
	do    func(d Driver, res map[string]string) error
}

const (
	email    = "test@agemu.dev"
	password = "hunter2"
	// Bath Towel $32.00 + 2 x Desk Lamp $29.99
	wantTotal = "Total $91.98"
)

var plan = []step{
	{"Open the app", func(d Driver, _ map[string]string) error {
		return d.Launch(Cond{Text: "Sign in"})
	}},
	{"Enter the e-mail", func(d Driver, _ map[string]string) error {
		return d.Fill("E-mail", email, Cond{})
	}},
	{"Enter the password", func(d Driver, _ map[string]string) error {
		return d.Fill("Password", password, Cond{})
	}},
	{"Sign in", func(d Driver, _ map[string]string) error {
		if err := d.Tap("Sign in", Cond{Text: "Search products"}); err != nil {
			return err
		}
		return d.Screenshot("catalog")
	}},
	{"Scroll to Bath Towel", func(d Driver, _ map[string]string) error {
		return d.ScrollUntil("Bath Towel")
	}},
	{"Open Bath Towel", func(d Driver, _ map[string]string) error {
		return d.Tap("Bath Towel", Cond{Text: "Add to cart"})
	}},
	{"Add it to the cart", func(d Driver, _ map[string]string) error {
		if err := d.Tap("Add to cart", Cond{Contains: "cart: 1"}); err != nil {
			return err
		}
		return d.Screenshot("towel")
	}},
	{"Back to the catalog", func(d Driver, _ map[string]string) error {
		return d.Tap("‹ Catalog", Cond{Text: "Search products"})
	}},
	{`Search for "lamp"`, func(d Driver, _ map[string]string) error {
		return d.Fill("Search products", "lamp", Cond{Text: "2 products"}) // the filter applied
	}},
	{"Open Desk Lamp", func(d Driver, _ map[string]string) error {
		return d.Tap("Desk Lamp", Cond{Text: "Add to cart"})
	}},
	{"Set quantity to 2", func(d Driver, _ map[string]string) error {
		return d.Tap("+", Cond{Text: "Quantity: 2"})
	}},
	{"Add 2 to the cart", func(d Driver, _ map[string]string) error {
		return d.Tap("Add to cart", Cond{Contains: "cart: 3"})
	}},
	{"Open the cart", func(d Driver, _ map[string]string) error {
		if err := d.Tap("View cart", Cond{Contains: "Total $"}); err != nil {
			return err
		}
		return d.Screenshot("cart")
	}},
	{"Check the total", func(d Driver, res map[string]string) error {
		got, err := d.Read("Total $")
		if err != nil {
			return err
		}
		res["total"] = got
		if got != wantTotal {
			return fmt.Errorf("cart shows %q, want %q", got, wantTotal)
		}
		return nil
	}},
	{"Go to checkout", func(d Driver, _ map[string]string) error {
		return d.Tap("Checkout", Cond{Text: "Place order"})
	}},
	{"Fill in the shipping address", func(d Driver, _ map[string]string) error {
		for _, f := range [][2]string{{"Full name", "Ada Lovelace"}, {"Street address", "12 Analytical St"}, {"City", "London"}, {"Postal code", "N1 9GU"}} {
			if err := d.Fill(f[0], f[1], Cond{}); err != nil {
				return err
			}
		}
		return nil
	}},
	{"Place the order", func(d Driver, _ map[string]string) error {
		return d.Tap("Place order", Cond{Contains: "Order #"})
	}},
	{"Read the order number", func(d Driver, res map[string]string) error {
		got, err := d.Read("Order #")
		if err != nil {
			return err
		}
		res["order"] = strings.TrimSpace(got)
		return d.Screenshot("confirmation")
	}},
}

func stepNames() []string {
	n := make([]string, len(plan))
	for i, s := range plan {
		n[i] = s.label
	}
	return n
}

func runPlan(r *Recorder, d Driver) (map[string]string, error) {
	res := map[string]string{}
	for i, s := range plan {
		r.mu.Lock()
		r.step, r.label = i+1, s.label
		r.mu.Unlock()
		if err := s.do(d, res); err != nil {
			return res, fmt.Errorf("step %d (%s): %w", i+1, s.label, err)
		}
	}
	return res, nil
}
