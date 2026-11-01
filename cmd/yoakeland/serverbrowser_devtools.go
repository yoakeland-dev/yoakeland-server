package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The agent's mini DevTools: what Chrome's own panels would show, through the same CDP.

func (s *serverBrowser) allTabs() []*tab {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*tab(nil), s.tabs...)
}

func (s *serverBrowser) clearNet(t *tab) {
	s.mu.Lock()
	t.net, t.netByID = nil, nil
	s.mu.Unlock()
}

// onNetwork records requests per tab, newest last, at most maxNetReqs of them.
func (s *serverBrowser) onNetwork(sid, method string, raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
		Request   struct {
			URL, Method string
		} `json:"request"`
		RedirectResponse *struct{ Status int } `json:"redirectResponse"`
		Response         struct {
			Status int
		} `json:"response"`
		EncodedDataLength float64 `json:"encodedDataLength"`
		ErrorText         string  `json:"errorText"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.bySession[sid]
	if t == nil {
		return
	}
	if t.netByID == nil {
		t.netByID = map[string]*netReq{}
	}
	r := t.netByID[p.RequestID]
	switch method {
	case "Network.requestWillBeSent":
		if strings.HasPrefix(p.Request.URL, "data:") {
			return
		}
		if r != nil && p.RedirectResponse != nil { // the earlier hop of a redirect keeps its own row
			r.status, r.done = p.RedirectResponse.Status, true
		}
		r = &netReq{id: p.RequestID, method: p.Request.Method, url: p.Request.URL, kind: strings.ToLower(p.Type)}
		t.netByID[p.RequestID] = r
		t.net = append(t.net, r)
		if len(t.net) > maxNetReqs {
			for _, old := range t.net[:len(t.net)-maxNetReqs] {
				delete(t.netByID, old.id)
			}
			t.net = t.net[len(t.net)-maxNetReqs:]
		}
	case "Network.responseReceived":
		if r != nil {
			r.status = p.Response.Status
		}
	case "Network.loadingFinished":
		if r != nil {
			r.size, r.done = int64(p.EncodedDataLength), true
		}
	case "Network.loadingFailed":
		if r != nil {
			r.errText, r.done = strings.TrimPrefix(p.ErrorText, "net::"), true
		}
	}
}

// networkList is the requests since the tab's last navigation whose URL contains filter.
func (s *serverBrowser) networkList(t *tab, filter string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	n := 0
	for _, r := range t.net {
		if filter != "" && !strings.Contains(r.url, filter) {
			continue
		}
		n++
		state := fmt.Sprint(r.status)
		switch {
		case r.errText != "" && !(r.status > 0 && r.errText == "ERR_ABORTED"): // a body the page dropped, not a failure
			state = "FAILED " + r.errText
		case !r.done && r.status == 0:
			state = "pending"
		}
		size := ""
		if r.done && r.size > 0 {
			size = fmt.Sprintf(" %.1f KB", float64(r.size)/1024)
		}
		fmt.Fprintf(&b, "%s %s %s %s%s\n", state, r.method, r.kind, clipURL(r.url), size)
	}
	if n == 0 {
		return "No requests since the last navigation."
	}
	return fmt.Sprintf("%d requests since the last navigation:\n%s", n, b.String())
}

func clipURL(u string) string {
	if r := []rune(u); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return u
}

// dragTo presses on from, moves in steps to the element named by toRef and releases there.
// ponytail: mouse events only; native HTML5 drag-and-drop (draggable=true) needs Input.setInterceptDrags.
func (s *serverBrowser) dragTo(ctx context.Context, t *tab, from located, toRef string) error {
	if !from.Visible {
		return errors.New("that element can't be reached with the pointer; scroll it into view")
	}
	to, err := s.locate(ctx, t, toRef)
	if err != nil {
		return err
	}
	if !to.Visible {
		return errors.New("the drop target can't be reached with the pointer")
	}
	mouse := func(typ string, x, y, buttons int) error {
		return s.do(ctx, t.sid, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "buttons": buttons, "clickCount": 1}, nil)
	}
	if err := mouse("mouseMoved", from.X, from.Y, 0); err != nil {
		return err
	}
	if err := mouse("mousePressed", from.X, from.Y, 1); err != nil {
		return err
	}
	const steps = 8
	for i := 1; i <= steps; i++ {
		if err := mouse("mouseMoved", from.X+(to.X-from.X)*i/steps, from.Y+(to.Y-from.Y)*i/steps, 1); err != nil {
			return err
		}
	}
	return mouse("mouseReleased", to.X, to.Y, 0)
}

const maxUploadBytes = 50 << 20

// upload attaches files on the server to the file input named by ref.
func (s *serverBrowser) upload(ctx context.Context, t *tab, ref string, files []string) error {
	for _, f := range files {
		st, err := os.Stat(f)
		switch {
		case !filepath.IsAbs(f):
			return fmt.Errorf("upload needs absolute paths, got %q", f)
		case err != nil:
			return fmt.Errorf("can't read %s: %w", f, err)
		case !st.Mode().IsRegular() || st.Size() > maxUploadBytes:
			return fmt.Errorf("%s isn't a regular file under %d MB", f, maxUploadBytes>>20)
		}
	}
	var obj struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails *struct{} `json:"exceptionDetails"`
	}
	expr := `(() => { const el = window.__yoakeRefs && window.__yoakeRefs.get(` + jsStr(ref) + `); return el && el.isConnected && el.tagName === "INPUT" && el.type === "file" ? el : null })()`
	if err := s.do(ctx, t.sid, "Runtime.evaluate", map[string]any{"expression": expr}, &obj); err != nil {
		return err
	}
	if obj.Result.ObjectID == "" {
		return errors.New("that ref isn't a file input on the page; take a new snapshot")
	}
	return s.do(ctx, t.sid, "DOM.setFileInputFiles", map[string]any{"files": files, "objectId": obj.Result.ObjectID}, nil)
}
