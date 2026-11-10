// Package ptr provides bounded, asynchronous reverse DNS enrichment.
package ptr

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

type Resolver interface {
	LookupAddr(context.Context, string) ([]string, error)
}
type Config struct {
	Timeout, PositiveTTL, NegativeTTL time.Duration
	MaxConcurrent, MaxEntries         int
}
type Entry struct {
	Name              string
	LookedUp, Expires time.Time
	Negative          bool
}

// Address accepts literal, unscoped addresses, including private and documentation
// addresses. Scoped and link-local addresses are excluded: a zone cannot be inferred.
func Address(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.IsLoopback() {
		return netip.Addr{}, false
	}
	return a, true
}

// Name selects the lexicographically smallest valid hostname (case-insensitive,
// then case-sensitive). DNS names are restricted to ordinary ASCII host labels.
func Name(records []string) (string, bool) {
	if len(records) == 0 || len(records) > 64 {
		return "", false
	}
	names := make([]string, 0, len(records))
	for _, s := range records {
		s = strings.TrimSuffix(s, ".")
		if len(s) == 0 || len(s) > 253 {
			continue
		}
		valid := true
		for _, label := range strings.Split(s, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				valid = false
				break
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					valid = false
					break
				}
			}
		}
		if valid {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.ToLower(names[i]), strings.ToLower(names[j])
		if a == b {
			return names[i] < names[j]
		}
		return a < b
	})
	return names[0], true
}

type Service struct {
	cfg      Config
	resolver Resolver
	mu       sync.Mutex
	entries  map[netip.Addr]Entry
	pending  map[netip.Addr]bool
	retry    map[netip.Addr]time.Time
	queue    chan netip.Addr
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func New(c Config, r Resolver) *Service {
	if r == nil {
		r = net.DefaultResolver
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: c, resolver: r, entries: map[netip.Addr]Entry{}, pending: map[netip.Addr]bool{}, retry: map[netip.Addr]time.Time{}, queue: make(chan netip.Addr, c.MaxConcurrent*16), ctx: ctx, cancel: cancel}
	for i := 0; i < c.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}
func (s *Service) Get(a netip.Addr) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[a]
	if ok && !time.Now().Before(e.Expires) {
		delete(s.entries, a)
		return Entry{}, false
	}
	return e, ok
}
func (s *Service) Schedule(a netip.Addr) bool {
	if _, ok := Address(a.String()); !ok {
		return false
	}
	a = a.Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.ctx.Err() != nil || s.pending[a] || now.Before(s.retry[a]) {
		return false
	}
	delete(s.retry, a)
	if e, ok := s.entries[a]; ok && now.Before(e.Expires) {
		return false
	}
	select {
	case s.queue <- a:
		s.pending[a] = true
		return true
	default:
		return false
	}
}
func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.ctx.Done():
			return
		case a := <-s.queue:
			s.lookup(a)
		}
	}
}
func (s *Service) lookup(a netip.Addr) {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	records, err := s.resolver.LookupAddr(ctx, a.String())
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	cancel()
	e := Entry{}
	cache := false
	if err == nil {
		if len(records) == 0 {
			e.Negative = true
			cache = true
		} else {
			e.Name, cache = Name(records)
		}
	} else {
		var dns *net.DNSError
		if errors.As(err, &dns) && dns.IsNotFound && !dns.IsTimeout && !dns.IsTemporary {
			e.Negative = true
			cache = true
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, a)
	if s.ctx.Err() != nil {
		return
	}
	if !cache {
		if len(s.retry) >= s.cfg.MaxEntries {
			s.evictRetry()
		}
		s.retry[a] = now.Add(10 * time.Second)
		return
	}
	e.LookedUp = now
	ttl := s.cfg.PositiveTTL
	if e.Negative {
		ttl = s.cfg.NegativeTTL
	}
	e.Expires = now.Add(ttl)
	if _, ok := s.entries[a]; !ok && len(s.entries) >= s.cfg.MaxEntries {
		var oldest netip.Addr
		var at time.Time
		for k, v := range s.entries {
			if !oldest.IsValid() || v.LookedUp.Before(at) || (v.LookedUp.Equal(at) && k.Compare(oldest) < 0) {
				oldest, at = k, v.LookedUp
			}
		}
		delete(s.entries, oldest)
	}
	s.entries[a] = e
}
func (s *Service) evictRetry() {
	var oldest netip.Addr
	var at time.Time
	for k, v := range s.retry {
		if !oldest.IsValid() || v.Before(at) || (v.Equal(at) && k.Compare(oldest) < 0) {
			oldest, at = k, v
		}
	}
	delete(s.retry, oldest)
}
func (s *Service) Close() { s.cancel(); s.wg.Wait() }
