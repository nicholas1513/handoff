package claudesessions

import (
	"errors"
	"slices"

	"github.com/godbus/dbus/v5"
)

const (
	atspiAccessible = "org.a11y.atspi.Accessible"
	maxTreeDepth    = 24 // tabs sit a dozen levels down
	maxTreeNodes    = 2000
)

type atspiRef struct {
	Bus  string
	Path dbus.ObjectPath
}

// atspi reads the accessibility bus. GTK publishes each app's tree there: a frame per window, and in it a named
// node per tab, background tabs too, whose name follows the tab's title. No other node in Ghostty's tree has a name.
type atspi struct {
	conn    *dbus.Conn
	ghostty []int
}

func dialAtspi(ghostty []int) (*atspi, error) {
	session, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	var addr string
	if err := session.Object("org.a11y.Bus", "/org/a11y/bus").Call("org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return nil, err
	}
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, err
	}
	return &atspi{conn, ghostty}, nil
}

func (a *atspi) close() { a.conn.Close() }

func (a *atspi) children(r atspiRef) ([]atspiRef, error) {
	var out []atspiRef
	err := a.conn.Object(r.Bus, r.Path).Call(atspiAccessible+".GetChildren", 0).Store(&out)
	return out, err
}

func (a *atspi) name(r atspiRef) (string, error) {
	v, err := a.conn.Object(r.Bus, r.Path).GetProperty(atspiAccessible + ".Name")
	if err != nil {
		return "", err
	}
	s, _ := v.Value().(string)
	return s, nil
}

func (a *atspi) title(n tabNode) (string, error) {
	return a.name(atspiRef{n.bus, dbus.ObjectPath(n.path)})
}

// tabs lists every tab of the Ghostty apps, by window.
func (a *atspi) tabs() ([]tabNode, error) {
	apps, err := a.children(atspiRef{"org.a11y.atspi.Registry", "/org/a11y/atspi/accessible/root"})
	if err != nil {
		return nil, err
	}
	var out []tabNode
	win, seen := 0, false
	for _, app := range apps {
		var pid uint32
		if a.conn.BusObject().Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, app.Bus).Store(&pid) != nil ||
			!slices.Contains(a.ghostty, int(pid)) {
			continue
		}
		seen = true
		frames, err := a.children(app)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			nodes := maxTreeNodes
			tab := 0
			a.walk(f, 0, &nodes, func(r atspiRef, name string) {
				out = append(out, tabNode{win, tab, r.Bus, string(r.Path), name})
				tab++
			})
			win++
		}
	}
	if !seen {
		return nil, errors.New("Ghostty isn't on it")
	}
	return out, nil
}

// walk calls found for each named node below r, in tree order, and doesn't go below those.
func (a *atspi) walk(r atspiRef, depth int, nodes *int, found func(atspiRef, string)) {
	kids, err := a.children(r)
	if err != nil || depth > maxTreeDepth {
		return
	}
	for _, k := range kids {
		if *nodes--; *nodes < 0 {
			return
		}
		if n, err := a.name(k); err == nil && n != "" {
			found(k, n)
			continue
		}
		a.walk(k, depth+1, nodes, found)
	}
}
