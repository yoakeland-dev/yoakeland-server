package main

import (
	"io"
	"os"
	"strings"

	"yoakeland/internal/audit"
	"yoakeland/internal/protocol"
)

const readLogMax = 2 << 20 // bytes of yoakeland.log tail returned

// handleReadLog answers read_log with the tail of the server log. Local
// connections only: log lines can hold secrets, so they're redacted too.
func handleReadLog(ipc *protocol.IPCStream, network string) {
	if network == "tcp" {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{
			Message: "read_log isn't available from a paired device; read the log on the computer running the server",
		})
		return
	}
	logMu.Lock()
	r, _ := logFile.(*rotatingLog)
	logMu.Unlock()
	if r == nil {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: "the server isn't writing a log file"})
		return
	}
	text, truncated, err := tailFile(r.path, readLogMax)
	if err != nil {
		_ = ipc.SendEvent(protocol.EventError, "", "", "", protocol.ErrorData{Message: "couldn't read the log: " + err.Error()})
		return
	}
	_ = ipc.SendEvent(protocol.EventLogText, "", "", "", protocol.LogTextData{Text: audit.Redact(text), Truncated: truncated})
}

// tailFile returns the last max bytes of path, starting at a line boundary.
func tailFile(path string, max int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	trunc := st.Size() > max
	if trunc {
		if _, err := f.Seek(st.Size()-max, io.SeekStart); err != nil {
			return "", false, err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false, err
	}
	s := string(b)
	if trunc {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, trunc, nil
}
