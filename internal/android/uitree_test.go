package android

import (
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

const sampleTree = `{"w":720,"h":1600,"density":320,"rotation":0,"app":"com.android.settings","windows":[
 {"type":2,"layer":3,"active":false,"focused":false,"title":"Gboard","b":[0,1000,720,1600],"pkg":"com.google.android.inputmethod.latin",
  "root":{"cls":"android.widget.FrameLayout","b":[0,1000,720,1600],"f":1}},
 {"type":1,"layer":1,"active":true,"focused":true,"title":"Settings","b":[0,0,720,1600],"pkg":"com.android.settings",
  "root":{"cls":"android.widget.FrameLayout","b":[0,0,720,1600],"f":1,"c":[
    {"cls":"android.widget.EditText","text":"Search settings","hint":"Search settings","id":"com.android.settings:id/search_src_text","b":[40,100,680,180],"f":3683},
    {"cls":"android.widget.LinearLayout","b":[0,200,720,320],"f":33,"c":[
      {"cls":"android.widget.TextView","text":"Network & internet","id":"android:id/title","b":[100,220,600,260],"f":1}
    ]},
    {"cls":"android.widget.Switch","desc":"Wi-Fi","b":[600,400,700,460],"f":57,"state":"On"},
    {"cls":"android.widget.ImageButton","desc":"Navigate up","b":[0,0,96,96],"f":33},
    {"cls":"androidx.recyclerview.widget.RecyclerView","b":[0,200,720,1600],"f":129}
  ]}}
]}`

func TestParseTree(t *testing.T) {
	tr, err := parseTree([]byte(sampleTree))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Width != 720 || tr.Height != 1600 || tr.App != "com.android.settings" {
		t.Fatalf("tree header: %dx%d %s", tr.Width, tr.Height, tr.App)
	}
	if tr.Root.Role != "window" || len(tr.Root.Children) != 2 {
		t.Fatalf("root: %+v", tr.Root)
	}
	// Windows keep the agent's top-most-first order.
	if tr.Root.Children[0].Type != "window:input_method" || tr.Root.Children[1].Type != "window:application" {
		t.Fatalf("window order: %s, %s", tr.Root.Children[0].Type, tr.Root.Children[1].Type)
	}

	byRef := map[string]*api.Element{}
	tr.Walk(func(e *api.Element) { byRef[e.Ref] = e })
	if len(byRef) != 11 {
		t.Fatalf("got %d elements", len(byRef))
	}

	field := daemon.Find(tr, api.Selector{ID: "search_src_text"})
	if len(field) != 1 {
		t.Fatalf("search field not found")
	}
	f := field[0]
	// f=3683: enabled|focused|clickable|longclickable|editable|focusable|showingHint
	if f.Role != "textfield" || f.Text != "" || f.Label != "Search settings" || !f.Focused || !f.Enabled {
		t.Fatalf("field: %+v", f)
	}
	if f.Frame != (api.Rect{X: 40, Y: 100, W: 640, H: 80}) {
		t.Fatalf("frame: %+v", f.Frame)
	}

	sw := daemon.Find(tr, api.Selector{Text: "Wi-Fi"})
	if len(sw) != 1 || sw[0].Role != "switch" || sw[0].Checked == nil || !*sw[0].Checked || sw[0].Value != "On" {
		t.Fatalf("switch: %+v", sw)
	}
	if b := daemon.Find(tr, api.Selector{Text: "Navigate up"}); len(b) != 1 || b[0].Role != "button" || !b[0].Clickable {
		t.Fatalf("image button: %+v", b)
	}
	if c := daemon.Find(tr, api.Selector{Role: "cell"}); len(c) != 1 || c[0].Frame.Y != 200 {
		t.Fatalf("clickable row should be a cell: %+v", c)
	}
	if l := daemon.Find(tr, api.Selector{Role: "list"}); len(l) != 1 {
		t.Fatalf("recycler should be a list: %+v", l)
	}
	if x := daemon.Find(tr, api.Selector{Text: "Network & internet"}); len(x) != 1 || x[0].Role != "text" || x[0].ID != "android:id/title" {
		t.Fatalf("title text: %+v", x)
	}
}
