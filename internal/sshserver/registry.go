package sshserver

import (
	"errors"
	"sync"

	tea "charm.land/bubbletea/v2"

	"flap/internal/store"
	"flap/internal/tui"
)

var (
	errServerFull    = errors.New("server full")
	errTooManyForKey = errors.New("too many sessions for this player")
)

var noticeShutdown = tui.NoticeMsg{
	Text:   "The server is restarting. Your score will be saved.",
	Quit:   true,
	Reason: store.ReasonShutdown,
}

// entry is one running game.
type entry struct {
	key  string
	mu   sync.Mutex
	prog *tea.Program
	done chan struct{}
}

func (e *entry) setProgram(p *tea.Program) {
	e.mu.Lock()
	e.prog = p
	e.mu.Unlock()
}

// send delivers a message to the game without blocking the caller for
// longer than the game takes to exit.
func (e *entry) send(msg tea.Msg) {
	e.mu.Lock()
	p := e.prog
	e.mu.Unlock()
	if p == nil {
		return
	}
	go func() {
		select {
		case <-e.done:
		default:
			p.Send(msg) // returns once delivered or once the program exits
		}
	}()
}

// registry tracks running games and enforces the session limits.
type registry struct {
	mu        sync.Mutex
	max       int
	perKey    int
	entries   map[*entry]struct{}
	keyCounts map[string]int
}

func newRegistry(max, perKey int) *registry {
	return &registry{max: max, perKey: perKey, entries: map[*entry]struct{}{}, keyCounts: map[string]int{}}
}

// add reserves a slot. Guests (empty key) are only bound by the global cap.
func (r *registry) add(key string) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) >= r.max {
		return nil, errServerFull
	}
	if key != "" && r.keyCounts[key] >= r.perKey {
		return nil, errTooManyForKey
	}
	e := &entry{key: key, done: make(chan struct{})}
	r.entries[e] = struct{}{}
	if key != "" {
		r.keyCounts[key]++
	}
	return e, nil
}

func (r *registry) remove(e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[e]; !ok {
		return
	}
	delete(r.entries, e)
	close(e.done)
	if e.key != "" {
		if r.keyCounts[e.key]--; r.keyCounts[e.key] <= 0 {
			delete(r.keyCounts, e.key)
		}
	}
}

func (r *registry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *registry) notifyAll(msg tea.Msg) {
	r.mu.Lock()
	list := make([]*entry, 0, len(r.entries))
	for e := range r.entries {
		list = append(list, e)
	}
	r.mu.Unlock()
	for _, e := range list {
		e.send(msg)
	}
}
