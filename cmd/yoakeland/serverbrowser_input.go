package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"yoakeland/internal/protocol"
)

// A human's input to the server browser, and the agent's key presses, as CDP Input.* commands.

type cdpCmd struct {
	Method string
	Params map[string]any
}

type inputJob struct {
	sid  string
	cmds []cdpCmd
}

const (
	maxInputText = 4096
	modAlt       = 1
	modCtrl      = 2
	modMeta      = 4
	modShift     = 8
	maxWheel     = 5000
	maxHeldKeys  = 32
)

var buttonBits = map[string]int{"left": 1, "right": 2, "middle": 4}

// inputState remembers what the controller is holding down, so losing control can let go of it.
type inputState struct {
	vpW, vpH int
	scale    float64 // page pixels per picture pixel when the screencast is shrunk (0 means 1)
	buttons  int
	lastX    float64
	lastY    float64
	keys     map[string]cdpCmd // code|key -> its key-up
}

func (st *inputState) point(in protocol.BrowserInputData) (x, y float64, err error) {
	if in.X == nil || in.Y == nil || math.IsNaN(*in.X) || math.IsInf(*in.X, 0) || math.IsNaN(*in.Y) || math.IsInf(*in.Y, 0) {
		return 0, 0, errors.New("x and y must be finite numbers")
	}
	k := st.scale
	if k <= 0 {
		k = 1
	}
	return math.Min(math.Max(*in.X*k, 0), float64(max(st.vpW-1, 0))), math.Min(math.Max(*in.Y*k, 0), float64(max(st.vpH-1, 0))), nil
}

// apply validates in and returns the CDP commands for it.
func (st *inputState) apply(in protocol.BrowserInputData) ([]cdpCmd, error) {
	if in.Modifiers < 0 || in.Modifiers > 15 {
		return nil, errors.New("modifiers out of range")
	}
	switch in.Kind {
	case "mouse":
		x, y, err := st.point(in)
		if err != nil {
			return nil, err
		}
		bit := buttonBits[in.Button]
		if in.Button != "" && bit == 0 {
			return nil, fmt.Errorf("unknown button %q", in.Button)
		}
		if in.Clicks < 0 || in.Clicks > 3 {
			return nil, errors.New("clicks out of range")
		}
		clicks := max(in.Clicks, 1)
		st.lastX, st.lastY = x, y
		p := map[string]any{"x": x, "y": y, "modifiers": in.Modifiers}
		switch in.Type {
		case "down", "up":
			if bit == 0 {
				return nil, errors.New("mouse down/up needs a button")
			}
			p["button"], p["clickCount"] = in.Button, clicks
			if in.Type == "down" {
				st.buttons |= bit
				p["type"] = "mousePressed"
			} else {
				st.buttons &^= bit
				p["type"] = "mouseReleased"
			}
		case "move":
			p["type"], p["button"] = "mouseMoved", heldButton(st.buttons)
		default:
			return nil, fmt.Errorf("unknown mouse type %q", in.Type)
		}
		p["buttons"] = st.buttons
		return []cdpCmd{{"Input.dispatchMouseEvent", p}}, nil
	case "wheel":
		x, y, err := st.point(in)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(in.DX) || math.IsInf(in.DX, 0) || math.IsNaN(in.DY) || math.IsInf(in.DY, 0) {
			return nil, errors.New("deltas must be finite numbers")
		}
		return []cdpCmd{{"Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y, "modifiers": in.Modifiers,
			"deltaX": math.Max(-maxWheel, math.Min(maxWheel, in.DX*max(st.scale, 1))), "deltaY": math.Max(-maxWheel, math.Min(maxWheel, in.DY*max(st.scale, 1))),
		}}}, nil
	case "key":
		if in.Type != "down" && in.Type != "up" {
			return nil, fmt.Errorf("unknown key type %q", in.Type)
		}
		if in.Key == "" || len(in.Key) > 32 || !utf8.ValidString(in.Key) || len(in.Code) > 32 || !utf8.ValidString(in.Code) {
			return nil, errors.New("bad key")
		}
		id := in.Code + "|" + in.Key
		up := cdpCmd{"Input.dispatchKeyEvent", keyEvent(false, in.Key, in.Code, in.Modifiers)}
		if in.Type == "up" {
			delete(st.keys, id)
			return []cdpCmd{up}, nil
		}
		if st.keys == nil {
			st.keys = map[string]cdpCmd{}
		}
		if _, held := st.keys[id]; !held && len(st.keys) >= maxHeldKeys {
			return nil, errors.New("too many keys held down")
		}
		st.keys[id] = up
		return []cdpCmd{{"Input.dispatchKeyEvent", keyEvent(true, in.Key, in.Code, in.Modifiers)}}, nil
	case "text":
		if in.Text == "" || len(in.Text) > maxInputText || !utf8.ValidString(in.Text) {
			return nil, errors.New("text must be 1 to 4096 bytes of UTF-8")
		}
		return []cdpCmd{{"Input.insertText", map[string]any{"text": in.Text}}}, nil
	}
	return nil, fmt.Errorf("unknown input kind %q", in.Kind)
}

// releaseAll lets go of every button and key still held, and forgets them.
func (st *inputState) releaseAll() []cdpCmd {
	var out []cdpCmd
	for name, bit := range buttonBits {
		if st.buttons&bit != 0 {
			st.buttons &^= bit
			out = append(out, cdpCmd{"Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "x": st.lastX, "y": st.lastY, "button": name, "buttons": st.buttons, "clickCount": 1,
			}})
		}
	}
	for _, up := range st.keys {
		out = append(out, up)
	}
	st.keys = nil
	return out
}

func heldButton(mask int) string {
	for _, n := range []string{"left", "middle", "right"} {
		if mask&buttonBits[n] != 0 {
			return n
		}
	}
	return "none"
}

// ---- keys ----

type keyDef struct {
	vk   int
	code string
	text string // typed by pressing it (Enter), if any
}

var namedKeys = func() map[string]keyDef {
	m := map[string]keyDef{
		"Enter": {13, "Enter", "\r"}, "Tab": {9, "Tab", ""}, "Escape": {27, "Escape", ""}, "Backspace": {8, "Backspace", ""},
		"Delete": {46, "Delete", ""}, "Insert": {45, "Insert", ""}, "Home": {36, "Home", ""}, "End": {35, "End", ""},
		"PageUp": {33, "PageUp", ""}, "PageDown": {34, "PageDown", ""}, "ArrowLeft": {37, "ArrowLeft", ""},
		"ArrowUp": {38, "ArrowUp", ""}, "ArrowRight": {39, "ArrowRight", ""}, "ArrowDown": {40, "ArrowDown", ""},
		" ": {32, "Space", " "}, "Shift": {16, "ShiftLeft", ""}, "Control": {17, "ControlLeft", ""}, "Alt": {18, "AltLeft", ""},
		"Meta": {91, "MetaLeft", ""}, "CapsLock": {20, "CapsLock", ""},
	}
	for i := 1; i <= 12; i++ {
		m[fmt.Sprintf("F%d", i)] = keyDef{111 + i, fmt.Sprintf("F%d", i), ""}
	}
	return m
}()

// keyAliases are the names the agent (and Electron) use.
var keyAliases = map[string]string{
	"up": "ArrowUp", "down": "ArrowDown", "left": "ArrowLeft", "right": "ArrowRight", "space": " ", "esc": "Escape",
	"return": "Enter", "ctrl": "Control", "cmd": "Meta", "del": "Delete", "pgup": "PageUp", "pgdn": "PageDown",
}

func lookupKey(key, code string) keyDef {
	if d, ok := namedKeys[key]; ok {
		return d
	}
	if utf8.RuneCountInString(key) == 1 {
		c := strings.ToUpper(key)
		switch {
		case c[0] >= 'A' && c[0] <= 'Z':
			return keyDef{int(c[0]), "Key" + c, ""}
		case c[0] >= '0' && c[0] <= '9':
			return keyDef{int(c[0]), "Digit" + c, ""}
		}
	}
	return keyDef{0, code, ""}
}

// keyEvent is one Input.dispatchKeyEvent. Printable keys carry their text; a Ctrl/Meta shortcut
// is a raw key plus the editing command it means, which headless Chrome on Linux doesn't map itself.
func keyEvent(down bool, key, code string, mods int) map[string]any {
	d := lookupKey(key, code)
	if code != "" {
		d.code = code
	}
	p := map[string]any{"key": key, "code": d.code, "windowsVirtualKeyCode": d.vk, "nativeVirtualKeyCode": d.vk, "modifiers": mods}
	if !down {
		p["type"] = "keyUp"
		return p
	}
	text := d.text
	if text == "" && utf8.RuneCountInString(key) == 1 && mods&(modCtrl|modMeta|modAlt) == 0 {
		text = key
	}
	if text != "" {
		p["type"], p["text"], p["unmodifiedText"] = "keyDown", text, text
	} else {
		p["type"] = "rawKeyDown"
	}
	if c := editingCommand(key, mods); c != "" {
		p["commands"] = []string{c}
	}
	return p
}

func editingCommand(key string, mods int) string {
	if mods&(modCtrl|modMeta) == 0 {
		return ""
	}
	switch strings.ToLower(key) {
	case "a":
		return "selectAll"
	case "c":
		return "copy"
	case "x":
		return "cut"
	case "v":
		return "paste"
	case "z":
		if mods&modShift != 0 {
			return "redo"
		}
		return "undo"
	case "y":
		return "redo"
	}
	return ""
}

var comboMods = map[string]int{"ctrl": modCtrl, "control": modCtrl, "cmd": modMeta, "meta": modMeta, "alt": modAlt, "shift": modShift}

// pressCmds is the agent's "Enter", "a" or "Control+a": a key down and up.
func pressCmds(combo string) ([]cdpCmd, error) {
	parts := strings.Split(combo, "+")
	key := parts[len(parts)-1]
	if key == "" {
		key = "+"
	}
	mods := 0
	for _, m := range parts[:len(parts)-1] {
		mods |= comboMods[strings.ToLower(m)]
	}
	if a, ok := keyAliases[strings.ToLower(key)]; ok {
		key = a
	} else if _, ok := namedKeys[key]; !ok {
		for n := range namedKeys {
			if strings.EqualFold(n, key) {
				key = n
			}
		}
	}
	if _, named := namedKeys[key]; !named && utf8.RuneCountInString(key) != 1 {
		return nil, fmt.Errorf("unknown key %q", key)
	}
	return []cdpCmd{
		{"Input.dispatchKeyEvent", keyEvent(true, key, "", mods)},
		{"Input.dispatchKeyEvent", keyEvent(false, key, "", mods)},
	}, nil
}

// clickCmds is a left click at x,y.
func clickCmds(x, y int) []cdpCmd { return clickCmdsWith(x, y, "left", false) }

// clickCmdsWith presses and releases button ("" is left) at x, y, twice for a double click.
func clickCmdsWith(x, y int, button string, double bool) []cdpCmd {
	if button == "" {
		button = "left"
	}
	mask := map[string]int{"left": 1, "right": 2, "middle": 4}[button]
	mouse := func(typ string, buttons, n int) cdpCmd {
		return cdpCmd{"Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": button, "buttons": buttons, "clickCount": n}}
	}
	cmds := []cdpCmd{{"Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}}}
	for n := 1; n <= map[bool]int{false: 1, true: 2}[double]; n++ {
		cmds = append(cmds, mouse("mousePressed", mask, n), mouse("mouseReleased", 0, n))
	}
	return cmds
}
