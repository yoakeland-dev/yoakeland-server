package mcp

import (
	"bufio"
	"io"
	"strings"
)

// maxFrame bounds one JSON-RPC message from a server, whatever the transport:
// a server that sends more is broken or hostile, and holding it all would let
// it grow the daemon's memory without limit.
const maxFrame = 16 << 20

type sseEvent struct {
	Event string
	Data  string
	ID    string
}

// readSSE parses a Server-Sent Events stream, calling fn for each event that
// carries data until fn returns false, the stream ends, or a line is longer
// than maxFrame. Comment lines (keep-alives) are skipped.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	var ev sseEvent
	var data []string
	flush := func() bool {
		if len(data) == 0 {
			ev = sseEvent{}
			return true
		}
		ev.Data = strings.Join(data, "\n")
		out := ev
		ev, data = sseEvent{}, nil
		return fn(out)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case line[0] == ':':
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Event = value
			case "data":
				data = append(data, value)
			case "id":
				ev.ID = value
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush() // a stream that ends without the blank line still delivers its last event
	return nil
}
