// Package asn resolves descriptive ASN registration metadata asynchronously.
package asn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Resolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}
type Config struct {
	Timeout, PositiveTTL, NegativeTTL time.Duration
	MaxConcurrent, MaxEntries         int
	Persist                           bool
	Path                              string
}
type Entry struct {
	ASN         uint32    `json:"asn"`
	Description string    `json:"description,omitempty"`
	Country     string    `json:"country,omitempty"`
	Registry    string    `json:"registry,omitempty"`
	Allocated   string    `json:"allocated,omitempty"`
	LookedUp    time.Time `json:"looked_up"`
	Expires     time.Time `json:"expires"`
	Negative    bool      `json:"negative"`
}

// Eligible excludes special-use, private and reserved ASNs, including AS_TRANS,
// documentation ASNs, and the reserved last values in each number space.
func Eligible(n uint32) bool {
	return n != 0 && n != 23456 && !(n >= 64496 && n <= 65551) && !(n >= 65552 && n <= 131071) && !(n >= 4200000000)
}
func Parse(n uint32, records []string) (Entry, error) {
	var found *Entry
	for _, record := range records {
		p := strings.SplitN(record, "|", 5)
		if len(p) != 5 {
			return Entry{}, errors.New("malformed TXT record")
		}
		id, err := strconv.ParseUint(strings.TrimSpace(p[0]), 10, 32)
		if err != nil || uint32(id) != n {
			return Entry{}, errors.New("mismatched TXT record")
		}
		country, registry, date, description := strings.TrimSpace(p[1]), strings.TrimSpace(p[2]), strings.TrimSpace(p[3]), strings.TrimSpace(p[4])
		if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' || registry == "" || len(registry) > 32 || description == "" || len(description) > 1024 {
			return Entry{}, errors.New("invalid TXT fields")
		}
		if _, err = time.Parse("2006-01-02", date); err != nil {
			return Entry{}, errors.New("invalid allocation date")
		}
		e := Entry{ASN: n, Country: country, Registry: registry, Allocated: date, Description: description}
		if found != nil {
			return Entry{}, errors.New("ambiguous TXT records")
		}
		found = &e
	}
	if found == nil {
		return Entry{}, errors.New("no valid TXT record")
	}
	return *found, nil
}

type cacheFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type cacheFS struct {
	createTemp func(string, string) (cacheFile, error)
	rename     func(string, string) error
	remove     func(string) error
}

func defaultCacheFS() cacheFS {
	return cacheFS{
		createTemp: func(dir, pattern string) (cacheFile, error) { return os.CreateTemp(dir, pattern) },
		rename:     os.Rename, remove: os.Remove,
	}
}

type Service struct {
	fs       cacheFS
	cfg      Config
	resolver Resolver
	mu       sync.Mutex
	entries  map[uint32]Entry
	pending  map[uint32]bool
	queue    chan uint32
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	dirty    bool
	retry    map[uint32]time.Time
}

func New(c Config, r Resolver) *Service {
	if r == nil {
		r = net.DefaultResolver
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{fs: defaultCacheFS(), cfg: c, resolver: r, entries: make(map[uint32]Entry), pending: make(map[uint32]bool), retry: make(map[uint32]time.Time), queue: make(chan uint32, c.MaxConcurrent*16), ctx: ctx, cancel: cancel}
	if c.Persist {
		s.load()
	}
	for i := 0; i < c.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	if c.Persist {
		s.wg.Add(1)
		go s.persistLoop()
	}
	return s
}
func (s *Service) Get(n uint32) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[n]
	if ok && !time.Now().Before(e.Expires) {
		delete(s.entries, n)
		s.dirty = true
		return Entry{}, false
	}
	return e, ok
}

// Schedule never waits for DNS or queue capacity; true means newly scheduled.
func (s *Service) Schedule(n uint32) bool {
	if !Eligible(n) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || s.pending[n] {
		return false
	}
	if until, ok := s.retry[n]; ok {
		if time.Now().Before(until) {
			return false
		}
		delete(s.retry, n)
	}
	if e, ok := s.entries[n]; ok && time.Now().Before(e.Expires) {
		return false
	}
	select {
	case s.queue <- n:
		s.pending[n] = true
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
		case n := <-s.queue:
			s.lookup(n)
		}
	}
}
func (s *Service) lookup(n uint32) {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	records, err := s.resolver.LookupTXT(ctx, fmt.Sprintf("AS%d.asn.cymru.com", n))
	cancel()
	var e Entry
	cache := false
	if err == nil {
		e, err = Parse(n, records)
		cache = err == nil
	} else {
		var dns *net.DNSError
		if errors.As(err, &dns) && dns.IsNotFound {
			e = Entry{ASN: n, Negative: true}
			cache = true
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, n)
	if s.ctx.Err() != nil {
		return
	}
	if !cache {
		if len(s.retry) >= s.cfg.MaxEntries {
			var oldest uint32
			var when time.Time
			for k, v := range s.retry {
				if when.IsZero() || v.Before(when) || (v.Equal(when) && k < oldest) {
					oldest, when = k, v
				}
			}
			delete(s.retry, oldest)
		}
		s.retry[n] = now.Add(10 * time.Second)
		return
	}
	e.LookedUp = now
	ttl := s.cfg.PositiveTTL
	if e.Negative {
		ttl = s.cfg.NegativeTTL
	}
	e.Expires = now.Add(ttl)
	s.put(e)
	s.dirty = true
}
func (s *Service) put(e Entry) {
	if len(s.entries) >= s.cfg.MaxEntries {
		var oldest Entry
		first := true
		for _, v := range s.entries {
			if first || v.LookedUp.Before(oldest.LookedUp) || (v.LookedUp.Equal(oldest.LookedUp) && v.ASN < oldest.ASN) {
				oldest = v
				first = false
			}
		}
		delete(s.entries, oldest.ASN)
	}
	s.entries[e.ASN] = e
}
func (s *Service) load() {
	f, err := os.Open(s.cfg.Path)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return
	}
	var entries []Entry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	now := time.Now()
	sort.Slice(entries, func(i, j int) bool { return entries[i].LookedUp.Before(entries[j].LookedUp) })
	for _, e := range entries {
		ttl := s.cfg.PositiveTTL
		if e.Negative {
			ttl = s.cfg.NegativeTTL
		}
		if Eligible(e.ASN) && e.Expires.After(now) && !e.LookedUp.IsZero() && !e.LookedUp.After(now) && e.Expires.After(e.LookedUp) && e.Expires.Sub(e.LookedUp) <= ttl && len(e.Description) <= 1024 && len(e.Country) <= 2 && len(e.Registry) <= 32 && len(e.Allocated) <= 10 && (e.Negative || e.Description != "") {
			s.put(e)
		}
	}
}
func (s *Service) flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.dirty = false
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ASN < entries[j].ASN })
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	f, err := s.fs.createTemp(filepath.Dir(s.cfg.Path), ".asn-cache-*")
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	name := f.Name()
	defer s.fs.remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = s.fs.rename(name, s.cfg.Path)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}
func (s *Service) persistLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.flush()
		}
	}
}
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
	if s.cfg.Persist {
		_ = s.flush()
	}
}
