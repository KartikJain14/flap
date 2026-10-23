package sshserver

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"flap/internal/security"
)

// inputReader feeds keyboard input from the SSH channel to the game. It
// paces reads with a token bucket so a client flooding input is slowed down
// by SSH flow control instead of consuming CPU, and it records when the
// player last pressed a key for the idle timeout.
type inputReader struct {
	r      io.Reader
	ctx    context.Context
	bucket *security.TokenBucket
	last   atomic.Int64 // unix nanos of the last input
	total  *atomic.Int64
	onEOF  func()
}

func newInputReader(ctx context.Context, r io.Reader, rate, burst int, total *atomic.Int64, onEOF func()) *inputReader {
	in := &inputReader{r: r, ctx: ctx, bucket: security.NewTokenBucket(float64(rate), burst, time.Now()), total: total, onEOF: onEOF}
	in.last.Store(time.Now().UnixNano())
	return in
}

const maxRead = 1024

func (in *inputReader) Read(p []byte) (int, error) {
	if len(p) > maxRead {
		p = p[:maxRead]
	}
	n, err := in.r.Read(p)
	if n > 0 {
		now := time.Now()
		in.last.Store(now.UnixNano())
		in.total.Add(int64(n))
		if wait := in.bucket.Take(n, now); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-in.ctx.Done():
				t.Stop()
			}
		}
	}
	if err != nil && in.onEOF != nil {
		in.onEOF()
	}
	return n, err
}

// idle returns how long ago the player last sent input.
func (in *inputReader) idle() time.Duration {
	return time.Since(time.Unix(0, in.last.Load()))
}

// outputWriter forwards frames to the SSH channel and lets a watchdog see a
// write that has been blocked for too long, which means the client stopped
// reading.
//
// It also performs the newline translation (ONLCR) that a server-side PTY
// would normally do. The renderer assumes "\n" returns the cursor to column
// 0, but the client's terminal is in raw mode and treats a bare LF as "down
// one line", so every LF is sent as CR LF. That is correct for every client,
// including Windows consoles that may add the CR themselves.
type outputWriter struct {
	w       io.Writer
	mu      sync.Mutex
	since   atomic.Int64 // unix nanos when the current write started; 0 if idle
	total   *atomic.Int64
	scratch []byte
}

// onlcr appends p to dst with every LF expanded to CR LF.
func onlcr(dst, p []byte) []byte {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			return append(dst, p...)
		}
		dst = append(dst, p[:i]...)
		dst = append(dst, '\r', '\n')
		p = p[i+1:]
	}
	return dst
}

func (o *outputWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scratch = onlcr(o.scratch[:0], p)
	if cap(o.scratch) > 1<<20 {
		// Do not pin a huge buffer after a one-off large frame.
		defer func() { o.scratch = nil }()
	}
	o.since.Store(time.Now().UnixNano())
	n, err := o.w.Write(o.scratch)
	o.since.Store(0)
	o.total.Add(int64(n))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// stalled reports how long the current write has been blocked.
func (o *outputWriter) stalled() time.Duration {
	s := o.since.Load()
	if s == 0 {
		return 0
	}
	return time.Since(time.Unix(0, s))
}
