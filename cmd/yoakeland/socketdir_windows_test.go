package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"
	"yoakeland/internal/sockpath"

	"golang.org/x/sys/windows"
)

// TestSetupLocalListener_PipeIsOwnerOnly checks the pipe's real security
// descriptor (not just the SDDL we asked for): owned by this user, a
// protected DACL with exactly one ACE, for this user — plus that a second
// instance refuses to start, and that sockpath.Dial accepts our own pipe.
func TestSetupLocalListener_PipeIsOwnerOnly(t *testing.T) {
	path := fmt.Sprintf(`%syoakeland-test-%d`, sockpath.PipePrefix, time.Now().UnixNano())
	ln, err := setupLocalListener(path, true)
	if err != nil {
		t.Fatalf("setupLocalListener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	c, err := sockpath.Dial(path)
	if err != nil {
		t.Fatalf("sockpath.Dial on our own pipe: %v", err)
	}
	h := windows.Handle(c.(interface{ Fd() uintptr }).Fd())
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	c.Close()
	if err != nil {
		t.Fatalf("GetSecurityInfo: %v", err)
	}
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if owner, _, _ := sd.Owner(); !owner.Equals(me.User.Sid) {
		t.Errorf("pipe owner = %s, want %s", owner, me.User.Sid)
	}
	if ctrl, _, _ := sd.Control(); ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("pipe DACL isn't protected (inherits ACEs): %s", sd)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("pipe has no DACL (= everyone allowed): %v", err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("pipe DACL has %d ACEs, want exactly 1 (this user): %s", dacl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	aceSid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !aceSid.Equals(me.User.Sid) {
		t.Errorf("pipe DACL = %s, want a single allow ACE for %s", sd, me.User.Sid)
	}

	if _, err := setupLocalListener(path, true); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("second listener on a live pipe: err = %v, want 'already listening'", err)
	}
}
