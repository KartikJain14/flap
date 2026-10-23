package security

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	ok := []string{"Ada", "pixel_pilot", "x-wing.99", "ABCDEFGHIJKLMNOP", "007bond"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v", n, err)
		}
	}
	bad := map[string]error{
		"ab":                ErrNameLength,
		"abcdefghijklmnopq": ErrNameLength,
		"has space":         ErrNameChars,
		"\x1b[31mred":       ErrNameChars,
		"bidi\u202ename":    ErrNameChars,
		"zero\u200bwidth":   ErrNameChars,
		"Ünïcode":           ErrNameChars,
		"日本語":               ErrNameChars,
		"_lead":             ErrNameStart,
		"admin":             ErrNameReserved,
		"Adm1n":             ErrNameReserved,
		"guest-1234":        ErrNameReserved,
		"GUEST_x":           ErrNameReserved,
		"r.o.o.t":           ErrNameReserved,
		"bad\xffutf8":       ErrNameChars,
	}
	for n, want := range bad {
		if err := ValidateName(n); err != want {
			t.Errorf("ValidateName(%q) = %v, want %v", n, err, want)
		}
	}
}

func TestNameKeyCollidesLookalikes(t *testing.T) {
	groups := [][]string{
		{"Alice", "ALICE", "A1ice", "al1ce", "a.lice", "A_L_I_C_E"},
		{"Bob0", "BOBO", "b0bo"},
	}
	for _, g := range groups {
		for _, n := range g[1:] {
			if NameKey(n) != NameKey(g[0]) {
				t.Errorf("NameKey(%q)=%q differs from NameKey(%q)=%q", n, NameKey(n), g[0], NameKey(g[0]))
			}
		}
	}
	if NameKey("alice") == NameKey("alicia") {
		t.Error("distinct names collided")
	}
}

func TestSanitizeName(t *testing.T) {
	if got := SanitizeName("\x1b[31mEvil Name!!\u202e"); got != "31mEvilName" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeName(strings.Repeat("a", 100)); len(got) != MaxNameLen {
		t.Fatalf("len %d", len(got))
	}
}

func TestLogString(t *testing.T) {
	got := LogString("user\n{\"level\":\"error\"}\x1b[2J", 64)
	if strings.ContainsAny(got, "\n\x1b") {
		t.Fatalf("unsafe log string %q", got)
	}
	if got := LogString(strings.Repeat("x", 100), 10); got != strings.Repeat("x", 10)+"…" {
		t.Fatalf("truncation: %q", got)
	}
}

func TestTokenBucket(t *testing.T) {
	t0 := time.Unix(1000, 0)
	b := NewTokenBucket(2, 3, t0)
	for i := 0; i < 3; i++ {
		if !b.Allow(t0) {
			t.Fatalf("burst token %d denied", i)
		}
	}
	if b.Allow(t0) {
		t.Fatal("token granted beyond burst")
	}
	if !b.Allow(t0.Add(500 * time.Millisecond)) {
		t.Fatal("token not refilled")
	}
	if b.Full(t0.Add(500 * time.Millisecond)) {
		t.Fatal("bucket reported full")
	}
	if !b.Full(t0.Add(time.Hour)) {
		t.Fatal("bucket not full after an hour")
	}
	// Take paces a stream: 3 tokens available, take 5 -> 1 second debt.
	p := NewTokenBucket(2, 3, t0)
	if d := p.Take(5, t0); d != time.Second {
		t.Fatalf("Take wait = %v, want 1s", d)
	}
	// Debt is bounded by the burst size.
	if d := p.Take(1000, t0); d != 1500*time.Millisecond {
		t.Fatalf("Take wait = %v, want 1.5s", d)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func testAdmission(c *clock) *Admission {
	return NewAdmission(AdmissionConfig{
		MaxConnsPerIP:     2,
		RatePerIP:         1,
		BurstPerIP:        3,
		GlobalRate:        100,
		GlobalBurst:       100,
		FailuresBeforeBan: 3,
		FailureWindow:     time.Minute,
		BanDuration:       10 * time.Minute,
		MaxTracked:        4,
	}, c.now)
}

func TestAdmissionConcurrencyAndRate(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	a := testAdmission(c)
	r1, why := a.Admit("1.2.3.4")
	if why != Admitted {
		t.Fatal(why)
	}
	r2, _ := a.Admit("1.2.3.4")
	if _, why := a.Admit("1.2.3.4"); why != RejectIPConcurrency {
		t.Fatalf("third concurrent conn: %v", why)
	}
	r1()
	r1() // release is idempotent
	r3, why := a.Admit("1.2.3.4")
	if why != Admitted {
		t.Fatalf("after release: %v", why)
	}
	r2()
	r3()
	// The burst of 3 is spent; the next attempt is rate limited.
	if _, why := a.Admit("1.2.3.4"); why != RejectIPRate {
		t.Fatalf("rate: %v", why)
	}
	c.add(time.Second)
	r, why := a.Admit("1.2.3.4")
	if why != Admitted {
		t.Fatalf("after refill: %v", why)
	}
	r()
	// Other addresses are unaffected.
	r, why = a.Admit("5.6.7.8")
	if why != Admitted {
		t.Fatalf("other ip: %v", why)
	}
	r()
}

func TestAdmissionBansAfterFailures(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	a := testAdmission(c)
	rel, _ := a.Admit("9.9.9.9")
	rel()
	for i := 0; i < 2; i++ {
		if a.RecordFailure("9.9.9.9") {
			t.Fatal("banned too early")
		}
	}
	if !a.RecordFailure("9.9.9.9") {
		t.Fatal("not banned after 3 failures")
	}
	if _, why := a.Admit("9.9.9.9"); why != RejectBanned {
		t.Fatalf("banned ip admitted: %v", why)
	}
	c.add(11 * time.Minute)
	rel, why := a.Admit("9.9.9.9")
	if why != Admitted {
		t.Fatalf("ban did not expire: %v", why)
	}
	rel()
}

func TestAdmissionTrackingIsBounded(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	a := testAdmission(c)
	var releases []func()
	for _, ip := range []string{"a", "b", "c", "d"} {
		r, why := a.Admit(ip)
		if why != Admitted {
			t.Fatal(why)
		}
		releases = append(releases, r)
	}
	// Table is full of active entries: fail closed.
	if _, why := a.Admit("e"); why != RejectTrackingFull {
		t.Fatalf("got %v, want tracking_full", why)
	}
	for _, r := range releases {
		r()
	}
	// Once idle and refilled, entries are evicted and new keys fit again.
	c.add(time.Hour)
	r, why := a.Admit("e")
	if why != Admitted {
		t.Fatalf("after sweep: %v", why)
	}
	r()
	if n := a.Tracked(); n > 4 {
		t.Fatalf("tracking %d entries", n)
	}
}

func TestAdmissionConcurrentUse(t *testing.T) {
	a := NewAdmission(AdmissionConfig{MaxConnsPerIP: 1000, RatePerIP: 1e6, BurstPerIP: 1e6, GlobalRate: 1e6, GlobalBurst: 1e6,
		FailuresBeforeBan: 1e6, FailureWindow: time.Minute, BanDuration: time.Minute, MaxTracked: 100}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Go(func() {
			for j := 0; j < 200; j++ {
				if r, why := a.Admit("k"); why == Admitted {
					a.RecordFailure("k")
					r()
				}
			}
		})
	}
	wg.Wait()
	if a.Banned("k") {
		t.Fatal("unexpected ban")
	}
}

func TestAddrKeyAndRedaction(t *testing.T) {
	v4 := &net.TCPAddr{IP: net.ParseIP("203.0.113.77"), Port: 4000}
	v6a := &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:aaaa::1"), Port: 1}
	v6b := &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:bbbb::9"), Port: 2}
	mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:203.0.113.77"), Port: 1}
	if AddrKey(v4) != "203.0.113.77" || AddrKey(mapped) != "203.0.113.77" {
		t.Fatalf("v4 keys %q %q", AddrKey(v4), AddrKey(mapped))
	}
	if AddrKey(v6a) != AddrKey(v6b) || AddrKey(v6a) != "2001:db8:1:2::/64" {
		t.Fatalf("v6 keys %q %q", AddrKey(v6a), AddrKey(v6b))
	}
	key := []byte("k")
	cases := map[string]string{
		IPLogFull:      "203.0.113.77",
		IPLogTruncated: "203.0.113.0/24",
		IPLogNone:      "-",
	}
	for mode, want := range cases {
		if got := NewIPRedactor(mode, key).Redact(v4); got != want {
			t.Errorf("%s: %q, want %q", mode, got, want)
		}
	}
	h1 := NewIPRedactor(IPLogHashed, key).Redact(v4)
	h2 := NewIPRedactor(IPLogHashed, []byte("other")).Redact(v4)
	if !strings.HasPrefix(h1, "h:") || h1 == h2 || strings.Contains(h1, "203") {
		t.Fatalf("hashed: %q %q", h1, h2)
	}
	if got := NewIPRedactor(IPLogTruncated, key).Redact(v6a); got != "2001:db8:1::/48" {
		t.Fatalf("v6 truncated: %q", got)
	}
}

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(2)
	first, second, third := s.TryAcquire(), s.TryAcquire(), s.TryAcquire()
	if !first || !second || third {
		t.Fatal("semaphore capacity wrong")
	}
	s.Release()
	if s.InUse() != 1 || !s.TryAcquire() {
		t.Fatal("release failed")
	}
}
