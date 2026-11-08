package dev.agemu.shop;

import java.util.LinkedHashMap;
import java.util.Map;

/** The catalog and the cart, in memory: a force-stop starts the app signed out with an empty cart. */
final class Store {
    static final class Product {
        final String name, category, blurb;
        final int cents;
        Product(String name, String category, int cents, String blurb) {
            this.name = name; this.category = category; this.cents = cents; this.blurb = blurb;
        }
    }

    static final Product[] PRODUCTS = {
        new Product("Desk Lamp", "Lighting", 2999, "Warm 2700 K light on a weighted steel base. Tilts 90 degrees."),
        new Product("Floor Lamp", "Lighting", 8900, "A linen shade on a slim oak stand, 160 cm tall."),
        new Product("Ceramic Mug", "Kitchen", 1250, "Hand-glazed stoneware, 350 ml. Dishwasher safe."),
        new Product("Pour-Over Kettle", "Kitchen", 5400, "Gooseneck spout for a slow, even pour. 1 litre."),
        new Product("Linen Napkins", "Dining", 1800, "Set of four, stonewashed for softness."),
        new Product("Oak Cutting Board", "Kitchen", 3500, "End-grain oak, oiled by hand, 40 x 25 cm."),
        new Product("Cast Iron Pan", "Kitchen", 4200, "Pre-seasoned, 26 cm. Goes from stove to oven."),
        new Product("Glass Carafe", "Dining", 2200, "Borosilicate glass with a cork stopper, 1 litre."),
        new Product("Throw Pillow", "Living", 2700, "Washed cotton cover with a feather insert."),
        new Product("Scented Candle", "Living", 1600, "Cedar and fig, soy wax, 40 hours of burn time."),
        new Product("Plant Pot", "Garden", 1900, "Terracotta with a drainage saucer, 18 cm."),
        new Product("Wall Clock", "Living", 3900, "Silent sweep movement in a birch frame."),
        new Product("Bookends", "Office", 2400, "A pair of powder-coated steel bookends."),
        new Product("Desk Organizer", "Office", 2100, "Walnut tray with three compartments."),
        new Product("Notebook Set", "Office", 1400, "Three dot-grid notebooks, A5, lay-flat binding."),
        new Product("Fountain Pen", "Office", 4800, "Steel nib, brass body, refillable converter."),
        new Product("Reading Glasses", "Accessories", 2500, "Light acetate frames, +1.5 lenses."),
        new Product("Bath Towel", "Bath", 3200, "Long-staple cotton, 600 g/m2, 70 x 140 cm."),
        new Product("Shower Caddy", "Bath", 2800, "Rust-proof stainless steel, hangs over the head."),
        new Product("Door Mat", "Home", 2300, "Coir fibre on a rubber backing, 60 x 40 cm."),
        new Product("Umbrella", "Accessories", 3100, "Wind-resistant frame, opens with one button."),
        new Product("Travel Mug", "Kitchen", 2600, "Vacuum insulated, keeps coffee hot for 6 hours."),
        new Product("Wool Blanket", "Living", 7900, "Merino wool throw, 130 x 180 cm, woven in Portugal."),
        new Product("Picnic Basket", "Garden", 5800, "Willow basket with plates and cutlery for four."),
        new Product("Linen Duvet", "Bedroom", 12900, "Stonewashed linen, double size, with two pillowcases."),
        new Product("Reading Light", "Lighting", 3400, "A small ceramic lamp with a dimmer switch."),
        new Product("Wool Rug", "Living", 14500, "Hand-tufted wool, 160 x 230 cm."),
        new Product("Coat Hooks", "Home", 1700, "Three solid ash hooks on a brass rail."),
        new Product("Tea Towels", "Kitchen", 1500, "Set of three, cotton herringbone."),
        new Product("Salad Bowl", "Dining", 3800, "Turned acacia wood, 28 cm."),
        new Product("Water Glasses", "Dining", 2600, "Set of four, mouth-blown glass."),
        new Product("Desk Chair", "Office", 18900, "Oak frame, woven paper-cord seat."),
        new Product("Watering Can", "Garden", 2900, "Galvanised steel, 2 litres, long spout."),
        new Product("Garden Gloves", "Garden", 1300, "Goatskin palms, cotton backs."),
        new Product("Bath Mat", "Bath", 2100, "Cotton waffle weave, 50 x 80 cm."),
        new Product("Soap Dish", "Bath", 900, "Unglazed stoneware that drains."),
    };

    /** Product index -> quantity, in the order added. */
    static final Map<Integer, Integer> cart = new LinkedHashMap<>();
    static String user;

    static void add(int product, int qty) {
        Integer have = cart.get(product);
        cart.put(product, (have == null ? 0 : have) + qty);
    }

    static int count() {
        int n = 0;
        for (int q : cart.values()) n += q;
        return n;
    }

    static int totalCents() {
        int t = 0;
        for (Map.Entry<Integer, Integer> e : cart.entrySet()) t += PRODUCTS[e.getKey()].cents * e.getValue();
        return t;
    }

    private Store() {}
}
