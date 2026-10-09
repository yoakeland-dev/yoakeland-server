package sandbox

import (
	"bytes"
	"fmt"
)

// maxCapturedOutput bounds each of a one-shot command's stdout and stderr:
// without it `yes` for the whole timeout fills the daemon's memory.
// It is the default: the runtime setting execOutputMaxBytes (SetLimits) overrides it.
const maxCapturedOutput = 1 << 20

// cappedBuffer keeps the first max bytes written and counts the rest. Write
// never fails, so the command isn't killed by a short write.
type cappedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int
}

func newCapped() *cappedBuffer { return &cappedBuffer{max: CurrentLimits().ExecOutputMaxBytes} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	c.dropped += len(p) - max(room, 0)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.dropped == 0 {
		return c.buf.String()
	}
	return c.buf.String() + fmt.Sprintf("\n[output truncated, %d bytes dropped]", c.dropped)
}
