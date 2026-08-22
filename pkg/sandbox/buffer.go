package sandbox

import "bytes"

// MaxExecOutput is how much of a command's output a sandbox keeps. Exec
// output is diagnostics, not data: a verbose restore tool must not OOM the
// drill process.
const MaxExecOutput = 4 << 20

// LimitedBuffer keeps at most Max bytes and silently discards the rest.
type LimitedBuffer struct {
	buf bytes.Buffer
	Max int
}

func (l *LimitedBuffer) Write(p []byte) (int, error) {
	if remaining := l.Max - l.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			l.buf.Write(p[:remaining])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil // report a full write so the stream keeps draining
}

func (l *LimitedBuffer) String() string { return l.buf.String() }
