// Command flapload is a bounded load generator for flapd. It opens many
// concurrent SSH game sessions that behave like players and reports
// connection latency, output bandwidth and, given the server PID, its CPU,
// memory, thread and file descriptor usage.
//
// Never point it at a production server you do not own.
//
//	flapload -addr 127.0.0.1:2222 -n 20 -duration 60s -pid $(pidof flapd)
//	flapload -addr 127.0.0.1:2222 -burst 200        # connection burst
//	flapload -addr 127.0.0.1:2222 -n 10 -cycle 3s   # connect/disconnect churn
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

type result struct {
	connect time.Duration
	err     error
}

type counters struct {
	bytes, sessions, failed atomic.Int64
	mu                      sync.Mutex
	latencies               []time.Duration
}

func (c *counters) add(r result) {
	if r.err != nil {
		c.failed.Add(1)
		return
	}
	c.sessions.Add(1)
	c.mu.Lock()
	c.latencies = append(c.latencies, r.connect)
	c.mu.Unlock()
}

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "server address")
	n := flag.Int("n", 10, "concurrent players")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	size := flag.String("size", "100x30", "terminal size WxH")
	ramp := flag.Duration("ramp", 2*time.Second, "spread connection starts over this period")
	cycle := flag.Duration("cycle", 0, "if set, each player reconnects after this long (churn test)")
	burst := flag.Int("burst", 0, "open this many connections at once, then report and exit")
	guest := flag.Bool("guest", false, "authenticate as guests (keyboard-interactive) instead of with keys")
	pid := flag.Int("pid", 0, "server PID to sample from /proc (Linux)")
	hostKey := flag.String("hostkey", "", "server host public key file (authorized_keys format); empty skips verification")
	flag.Parse()

	w, h, err := parseSize(*size)
	if err != nil {
		fatal(err)
	}
	hostCB, err := hostKeyCallback(*hostKey)
	if err != nil {
		fatal(err)
	}
	var c counters
	sampler := newSampler(*pid)

	if *burst > 0 {
		runBurst(*addr, *burst, w, h, *guest, hostCB, &c, sampler)
		return
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *n; i++ {
		delay := time.Duration(0)
		if *n > 1 {
			delay = *ramp * time.Duration(i) / time.Duration(*n-1)
		}
		wg.Go(func() {
			time.Sleep(delay)
			for {
				select {
				case <-stop:
					return
				default:
				}
				life := *duration
				if *cycle > 0 {
					life = *cycle
				}
				r := play(*addr, w, h, *guest, hostCB, life, stop, &c)
				c.add(r)
				if r.err != nil {
					time.Sleep(500 * time.Millisecond)
				}
				if *cycle == 0 {
					return
				}
			}
		})
	}

	start := time.Now()
	tick := time.NewTicker(time.Second)
	var lastBytes int64
	fmt.Printf("%6s %8s %8s %10s %8s %8s %7s %5s\n", "t", "sessions", "failed", "KB/s out", "CPU%", "RSS MB", "threads", "fds")
	for time.Since(start) < *duration {
		<-tick.C
		b := c.bytes.Load()
		s := sampler.sample()
		fmt.Printf("%6.0f %8d %8d %10.1f %8s %8s %7s %5s\n", time.Since(start).Seconds(), c.sessions.Load(), c.failed.Load(),
			float64(b-lastBytes)/1024, s.cpu, s.rss, s.threads, s.fds)
		lastBytes = b
	}
	close(stop)
	wg.Wait()
	tick.Stop()
	elapsed := time.Since(start).Seconds()
	report(&c, *n, elapsed)
	time.Sleep(2 * time.Second)
	if s := sampler.sample(); sampler.ok {
		fmt.Printf("after all sessions closed: RSS %s MB, threads %s, fds %s\n", s.rss, s.threads, s.fds)
	}
}

func report(c *counters, n int, elapsed float64) {
	c.mu.Lock()
	lat := append([]time.Duration(nil), c.latencies...)
	c.mu.Unlock()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(float64(len(lat))*p))]
	}
	fmt.Printf("\nsessions completed: %d, failed: %d\n", c.sessions.Load(), c.failed.Load())
	fmt.Printf("connect+auth+pty latency: p50 %v, p95 %v, max %v\n", pct(0.5).Round(time.Millisecond), pct(0.95).Round(time.Millisecond), pct(1).Round(time.Millisecond))
	fmt.Printf("output: %.1f KB/s total, %.1f KB/s per player\n", float64(c.bytes.Load())/1024/elapsed, float64(c.bytes.Load())/1024/elapsed/float64(max(n, 1)))
}

func clientConfig(guest bool, hostCB ssh.HostKeyCallback) (*ssh.ClientConfig, error) {
	cfg := &ssh.ClientConfig{User: "load", HostKeyCallback: hostCB, Timeout: 10 * time.Second}
	if guest {
		cfg.Auth = []ssh.AuthMethod{ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil })}
		return cfg, nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	return cfg, nil
}

// play runs one session that behaves like a player: start a run, flap at a
// human-ish rhythm with jitter, and restart after crashing.
func play(addr string, w, h int, guest bool, hostCB ssh.HostKeyCallback, life time.Duration, stop <-chan struct{}, c *counters) result {
	cfg, err := clientConfig(guest, hostCB)
	if err != nil {
		return result{err: err}
	}
	t0 := time.Now()
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return result{err: err}
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return result{err: err}
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", h, w, ssh.TerminalModes{}); err != nil {
		return result{err: err}
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return result{err: err}
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return result{err: err}
	}
	if err := sess.Shell(); err != nil {
		return result{err: err}
	}
	first := make([]byte, 4096)
	if _, err := io.ReadAtLeast(stdout, first, 1); err != nil {
		return result{err: fmt.Errorf("no output: %w", err)}
	}
	connect := time.Since(t0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			c.bytes.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	end := time.After(life)
	next := time.Now().Add(time.Second)
	for {
		select {
		case <-stop:
			stdin.Write([]byte("q"))
			<-done
			return result{connect: connect}
		case <-end:
			stdin.Write([]byte("q"))
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
			return result{connect: connect}
		case <-done:
			return result{connect: connect, err: errors.New("server closed the session")}
		case <-time.After(time.Until(next)):
			if _, err := stdin.Write([]byte(" ")); err != nil {
				return result{connect: connect, err: err}
			}
			next = time.Now().Add(time.Duration(480+mrand.IntN(220)) * time.Millisecond)
		}
	}
}

// runBurst opens many connections at once to observe admission control.
func runBurst(addr string, n, w, h int, guest bool, hostCB ssh.HostKeyCallback, c *counters, s *sampler) {
	before := s.sample()
	var wg sync.WaitGroup
	var ok, refused, other atomic.Int64
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Go(func() {
			cfg, err := clientConfig(guest, hostCB)
			if err != nil {
				other.Add(1)
				return
			}
			client, err := ssh.Dial("tcp", addr, cfg)
			if err != nil {
				refused.Add(1)
				return
			}
			defer client.Close()
			sess, err := client.NewSession()
			if err != nil {
				other.Add(1)
				return
			}
			defer sess.Close()
			sess.RequestPty("xterm-256color", h, w, nil)
			out, _ := sess.StdoutPipe()
			sess.Shell()
			buf := make([]byte, 64*1024)
			n, _ := io.ReadAtLeast(out, buf, 1)
			if strings.Contains(string(buf[:n]), "full right now") {
				refused.Add(1)
				return
			}
			ok.Add(1)
			time.Sleep(3 * time.Second)
		})
	}
	wg.Wait()
	during := s.sample()
	time.Sleep(3 * time.Second)
	after := s.sample()
	fmt.Printf("burst of %d connections in %v: %d got a game, %d refused (limits), %d other errors\n",
		n, time.Since(start).Round(time.Millisecond), ok.Load(), refused.Load(), other.Load())
	if s.ok {
		fmt.Printf("server RSS MB before/after burst/3s later: %s / %s / %s; fds: %s / %s / %s; threads: %s / %s / %s\n",
			before.rss, during.rss, after.rss, before.fds, during.fds, after.fds, before.threads, during.threads, after.threads)
	}
}

func parseSize(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, fmt.Errorf("size %q must look like 100x30", s)
	}
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || w < 1 || h < 1 {
		return 0, 0, fmt.Errorf("size %q must look like 100x30", s)
	}
	return w, h, nil
}

func hostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	if path == "" {
		fmt.Fprintln(os.Stderr, "flapload: host key not verified (pass -hostkey to pin it)")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, err
	}
	return ssh.FixedHostKey(key), nil
}

type sampler struct {
	pid      int
	ok       bool
	lastCPU  float64
	lastWall time.Time
}

type sample struct{ cpu, rss, threads, fds string }

func newSampler(pid int) *sampler {
	s := &sampler{pid: pid}
	if pid > 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			s.ok = true
			s.lastCPU, _ = s.cpuSeconds()
			s.lastWall = time.Now()
		}
	}
	return s
}

// cpuSeconds reads user+system time from /proc/PID/stat (clock ticks of
// 1/100 s on Linux).
func (s *sampler) cpuSeconds() (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.pid))
	if err != nil {
		return 0, err
	}
	str := string(data)
	str = str[strings.LastIndexByte(str, ')')+2:]
	f := strings.Fields(str)
	if len(f) < 13 {
		return 0, errors.New("short stat")
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / 100, nil
}

func (s *sampler) sample() sample {
	if !s.ok {
		return sample{"-", "-", "-", "-"}
	}
	var out sample
	cpu, err := s.cpuSeconds()
	now := time.Now()
	if err == nil {
		out.cpu = fmt.Sprintf("%.1f", (cpu-s.lastCPU)/now.Sub(s.lastWall).Seconds()*100)
		s.lastCPU, s.lastWall = cpu, now
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", s.pid)); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimSpace(v)
			switch k {
			case "VmRSS":
				kb, _ := strconv.Atoi(strings.Fields(v)[0])
				out.rss = fmt.Sprintf("%.1f", float64(kb)/1024)
			case "Threads":
				out.threads = v
			}
		}
	}
	if ents, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(s.pid), "fd")); err == nil {
		out.fds = strconv.Itoa(len(ents))
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "flapload:", err)
	os.Exit(1)
}
