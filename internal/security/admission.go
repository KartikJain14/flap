package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Rejection explains why a connection was not admitted.
type Rejection uint8

const (
	Admitted Rejection = iota
	RejectBanned
	RejectIPRate
	RejectGlobalRate
	RejectIPConcurrency
	RejectTrackingFull
)

func (r Rejection) String() string {
	switch r {
	case Admitted:
		return "admitted"
	case RejectBanned:
		return "banned"
	case RejectIPRate:
		return "ip_rate"
	case RejectGlobalRate:
		return "global_rate"
	case RejectIPConcurrency:
		return "ip_concurrency"
	case RejectTrackingFull:
		return "tracking_full"
	}
	return "unknown"
}

// AdmissionConfig configures per-address admission control.
type AdmissionConfig struct {
	// MaxConnsPerIP bounds concurrent connections from one address key.
	MaxConnsPerIP int
	// RatePerIP and BurstPerIP shape new connections per address key.
	RatePerIP  float64 // connections per second
	BurstPerIP int
	// GlobalRate and GlobalBurst shape new connections from everyone.
	GlobalRate  float64
	GlobalBurst int
	// FailuresBeforeBan authentication failures within FailureWindow ban the
	// address key for BanDuration.
	FailuresBeforeBan int
	FailureWindow     time.Duration
	BanDuration       time.Duration
	// MaxTracked bounds the memory used for per-address state. When full and
	// nothing can be evicted, new addresses are rejected (fail closed).
	MaxTracked int
}

type ipState struct {
	bucket      *TokenBucket
	active      int
	failures    int
	failStart   time.Time
	bannedUntil time.Time
}

// Admission decides whether new connections may proceed. It is safe for
// concurrent use.
type Admission struct {
	cfg    AdmissionConfig
	now    func() time.Time
	mu     sync.Mutex
	global *TokenBucket
	ips    map[string]*ipState
}

// NewAdmission returns an admission controller. now may be nil to use the
// wall clock.
func NewAdmission(cfg AdmissionConfig, now func() time.Time) *Admission {
	if now == nil {
		now = time.Now
	}
	return &Admission{
		cfg:    cfg,
		now:    now,
		global: NewTokenBucket(cfg.GlobalRate, cfg.GlobalBurst, now()),
		ips:    make(map[string]*ipState),
	}
}

// Admit registers a new connection from the given address key. On success it
// returns a release function that must be called exactly once when the
// connection closes.
func (a *Admission) Admit(key string) (release func(), why Rejection) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	st, ok := a.ips[key]
	if !ok {
		if len(a.ips) >= a.cfg.MaxTracked {
			a.sweepLocked(now)
			if len(a.ips) >= a.cfg.MaxTracked {
				return nil, RejectTrackingFull
			}
		}
		st = &ipState{bucket: NewTokenBucket(a.cfg.RatePerIP, a.cfg.BurstPerIP, now)}
		a.ips[key] = st
	}
	switch {
	case now.Before(st.bannedUntil):
		return nil, RejectBanned
	case st.active >= a.cfg.MaxConnsPerIP:
		return nil, RejectIPConcurrency
	case !st.bucket.Allow(now):
		return nil, RejectIPRate
	case !a.global.Allow(now):
		return nil, RejectGlobalRate
	}
	st.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			st.active--
		})
	}, Admitted
}

// RecordFailure counts an authentication failure for the address key and
// reports whether the key is now banned.
func (a *Admission) RecordFailure(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	st, ok := a.ips[key]
	if !ok {
		return false
	}
	if now.Sub(st.failStart) > a.cfg.FailureWindow {
		st.failStart, st.failures = now, 0
	}
	st.failures++
	if st.failures >= a.cfg.FailuresBeforeBan {
		st.bannedUntil = now.Add(a.cfg.BanDuration)
		st.failures = 0
		return true
	}
	return false
}

// Banned reports whether the key is currently banned.
func (a *Admission) Banned(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.ips[key]
	return ok && a.now().Before(st.bannedUntil)
}

// Sweep forgets address state that no longer carries information. It runs
// automatically when the table is full; calling it periodically keeps memory
// low.
func (a *Admission) Sweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(a.now())
}

// Tracked returns the number of address keys currently tracked.
func (a *Admission) Tracked() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.ips)
}

func (a *Admission) sweepLocked(now time.Time) {
	for k, st := range a.ips {
		if st.active == 0 && !now.Before(st.bannedUntil) &&
			(st.failures == 0 || now.Sub(st.failStart) > a.cfg.FailureWindow) &&
			st.bucket.Full(now) {
			delete(a.ips, k)
		}
	}
}

// Semaphore is a non-blocking counting semaphore.
type Semaphore struct{ ch chan struct{} }

// NewSemaphore returns a semaphore with n slots.
func NewSemaphore(n int) *Semaphore { return &Semaphore{ch: make(chan struct{}, n)} }

// TryAcquire takes a slot if one is free.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot taken by TryAcquire.
func (s *Semaphore) Release() { <-s.ch }

// InUse returns the number of slots taken.
func (s *Semaphore) InUse() int { return len(s.ch) }

// AddrKey returns the key used to group connections from the same source.
// IPv4 addresses are used as-is; IPv6 addresses are grouped by /64 because a
// single host usually controls a whole /64.
func AddrKey(addr net.Addr) string {
	ip, ok := addrIP(addr)
	if !ok {
		return "unknown"
	}
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64)
	return p.String()
}

func addrIP(addr net.Addr) (netip.Addr, bool) {
	if addr == nil {
		return netip.Addr{}, false
	}
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	default:
		ap, err := netip.ParseAddrPort(addr.String())
		if err != nil {
			return netip.Addr{}, false
		}
		ip = ap.Addr()
	}
	if !ip.IsValid() {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// IP logging modes.
const (
	IPLogFull      = "full"
	IPLogTruncated = "truncated"
	IPLogHashed    = "hashed"
	IPLogNone      = "none"
)

// IPRedactor renders remote addresses for logs according to the configured
// privacy mode.
type IPRedactor struct {
	mode string
	key  []byte
}

// NewIPRedactor returns a redactor. key is used for the hashed mode and
// should be random per process so hashes cannot be reversed by enumerating
// the IPv4 space offline.
func NewIPRedactor(mode string, key []byte) IPRedactor {
	return IPRedactor{mode: mode, key: key}
}

// Redact formats addr for a log entry.
func (r IPRedactor) Redact(addr net.Addr) string {
	ip, ok := addrIP(addr)
	if !ok {
		return "unknown"
	}
	switch r.mode {
	case IPLogFull:
		return ip.String()
	case IPLogTruncated:
		bits := 48
		if ip.Is4() {
			bits = 24
		}
		p, _ := ip.Prefix(bits)
		return p.String()
	case IPLogHashed:
		m := hmac.New(sha256.New, r.key)
		m.Write(ip.AsSlice())
		return "h:" + hex.EncodeToString(m.Sum(nil)[:8])
	default:
		return "-"
	}
}
