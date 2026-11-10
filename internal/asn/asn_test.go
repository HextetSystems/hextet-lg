package asn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string) ([]string, error)

func (f resolverFunc) LookupTXT(c context.Context, s string) ([]string, error) { return f(c, s) }
func cfg() Config {
	return Config{Timeout: time.Second, PositiveTTL: time.Hour, NegativeTTL: time.Hour, MaxConcurrent: 2, MaxEntries: 3}
}
func wait(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out")
}
func TestParseAndEligibility(t *testing.T) {
	e, err := Parse(13335, []string{"13335 | US | arin | 2010-07-14 | Example | extra"})
	if err != nil || e.Description != "Example | extra" || e.Country != "US" {
		t.Fatalf("%+v %v", e, err)
	}
	for _, v := range [][]string{{"13335 | US"}, {"13334 | US | arin | 2010-07-14 | wrong"}, {"bad | US | arin | 2010-07-14 | bad"}, {"13335 | US | arin | 2010-07-14 | one", "13335 | US | arin | 2010-07-14 | two"}} {
		if _, err := Parse(13335, v); err == nil {
			t.Fatalf("accepted %v", v)
		}
	}
	for _, n := range []uint32{0, 23456, 64496, 64512, 65535, 65552, 4200000000, 4294967295} {
		if Eligible(n) {
			t.Errorf("eligible %d", n)
		}
	}
	if !Eligible(13335) {
		t.Fatal("public ASN excluded")
	}
}
func TestCacheDedupAndFailures(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	r := resolverFunc(func(c context.Context, s string) ([]string, error) {
		calls.Add(1)
		select {
		case <-release:
		case <-c.Done():
			return nil, c.Err()
		}
		if strings.Contains(s, "13335") {
			return []string{"13335 | US | arin | 2010-07-14 | example"}, nil
		}
		return nil, &net.DNSError{IsNotFound: true}
	})
	s := New(cfg(), r)
	defer s.Close()
	if !s.Schedule(13335) {
		t.Fatal("not scheduled")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Schedule(13335) }()
	}
	wg.Wait()
	close(release)
	wait(t, func() bool { _, ok := s.Get(13335); return ok })
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	if s.Schedule(13335) {
		t.Fatal("cached lookup scheduled")
	}
	s.Schedule(15169)
	wait(t, func() bool { e, ok := s.Get(15169); return ok && e.Negative })
}
func TestTransientAndExpiry(t *testing.T) {
	c := cfg()
	c.PositiveTTL = 30 * time.Millisecond
	r := resolverFunc(func(_ context.Context, s string) ([]string, error) {
		if strings.Contains(s, "15169") {
			return nil, &net.DNSError{IsTimeout: true, IsTemporary: true}
		}
		return []string{"13335 | US | arin | 2010-07-14 | example"}, nil
	})
	s := New(c, r)
	defer s.Close()
	s.Schedule(13335)
	wait(t, func() bool { _, ok := s.Get(13335); return ok })
	time.Sleep(40 * time.Millisecond)
	if _, ok := s.Get(13335); ok {
		t.Fatal("expired")
	}
	s.Schedule(15169)
	wait(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.pending[15169] })
	if _, ok := s.Get(15169); ok {
		t.Fatal("transient cached")
	}
}
func TestCapacityAndPersistence(t *testing.T) {
	c := cfg()
	c.MaxEntries = 2
	c.Persist = true
	c.Path = filepath.Join(t.TempDir(), "asn.json")
	s := New(c, resolverFunc(func(_ context.Context, name string) ([]string, error) {
		var n uint32
		fmt.Sscanf(name, "AS%d.", &n)
		return []string{fmt.Sprintf("%d | US | arin | 2010-07-14 | name", n)}, nil
	}))
	for _, n := range []uint32{1, 2, 3} {
		s.Schedule(n)
		wait(t, func() bool { _, ok := s.Get(n); return ok })
	}
	if _, ok := s.Get(1); ok {
		t.Fatal("oldest not evicted")
	}
	s.Close()
	info, err := os.Stat(c.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", info, err)
	}
	s = New(c, nil)
	defer s.Close()
	if _, ok := s.Get(3); !ok {
		t.Fatal("not loaded")
	}
}
func TestShutdownCancelsLookup(t *testing.T) {
	started := make(chan struct{})
	s := New(cfg(), resolverFunc(func(ctx context.Context, _ string) ([]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	s.Schedule(1)
	<-started
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}
}
func TestBadPersistence(t *testing.T) {
	c := cfg()
	c.Persist = true
	c.Path = filepath.Join(t.TempDir(), "missing", "x")
	s := New(c, resolverFunc(func(context.Context, string) ([]string, error) { return nil, errors.New("offline") }))
	s.Schedule(1)
	s.Close()
}

func TestQueueAndConcurrentLimit(t *testing.T) {
	c := cfg()
	c.MaxConcurrent = 1
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var active, peak atomic.Int32
	s := New(c, resolverFunc(func(ctx context.Context, _ string) ([]string, error) {
		v := active.Add(1)
		if v > peak.Load() {
			peak.Store(v)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return nil, &net.DNSError{IsNotFound: true}
	}))
	defer s.Close()
	if !s.Schedule(1) {
		t.Fatal("first scheduling failed")
	}
	<-started
	for n := uint32(2); n <= 17; n++ {
		if !s.Schedule(n) {
			t.Fatalf("queue rejected %d", n)
		}
	}
	if s.Schedule(18) {
		t.Fatal("queue exceeded capacity")
	}
	if s.Schedule(2) {
		t.Fatal("duplicate pending")
	}
	close(release)
	wait(t, func() bool { _, ok := s.Get(17); return ok })
	if peak.Load() != 1 {
		t.Fatalf("concurrency: %d", peak.Load())
	}
}
func TestMalformedAndOversizedPersistence(t *testing.T) {
	c := cfg()
	c.Persist = true
	c.Path = filepath.Join(t.TempDir(), "cache.json")
	for _, data := range []string{"{", strings.Repeat("x", (16<<20)+1)} {
		if err := os.WriteFile(c.Path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(c, nil)
		if len(s.entries) != 0 {
			t.Fatal("invalid persisted data loaded")
		}
		s.Close()
	}
}

type brokenCacheFile struct {
	cacheFile
	mode string
}

func (f brokenCacheFile) Write(p []byte) (int, error) {
	if f.mode == "write" {
		n := min(4, len(p))
		_, _ = f.cacheFile.Write(p[:n])
		return n, errors.New("disk full")
	}
	return f.cacheFile.Write(p)
}
func (f brokenCacheFile) Sync() error {
	if f.mode == "sync" {
		return errors.New("interrupted sync")
	}
	return f.cacheFile.Sync()
}
func TestPersistenceFailurePreservesExistingFile(t *testing.T) {
	for _, mode := range []string{"create", "write", "sync", "rename"} {
		t.Run(mode, func(t *testing.T) {
			c := cfg()
			c.Persist = true
			c.Path = filepath.Join(t.TempDir(), "cache.json")
			original := []byte("[]")
			if err := os.WriteFile(c.Path, original, 0400); err != nil {
				t.Fatal(err)
			}
			s := New(c, nil)
			defer s.Close()
			now := time.Now()
			s.mu.Lock()
			s.put(Entry{ASN: 13335, Description: "name", LookedUp: now, Expires: now.Add(time.Hour)})
			s.dirty = true
			s.mu.Unlock()
			fs := defaultCacheFS()
			create := fs.createTemp
			fs.createTemp = func(dir, pattern string) (cacheFile, error) {
				if mode == "create" {
					return nil, os.ErrPermission
				}
				f, err := create(dir, pattern)
				if err != nil {
					return nil, err
				}
				return brokenCacheFile{f, mode}, nil
			}
			if mode == "rename" {
				fs.rename = func(string, string) error { return errors.New("rename failure") }
			}
			s.fs = fs
			if err := s.flush(); err == nil {
				t.Fatal("expected write error")
			}
			got, err := os.ReadFile(c.Path)
			if err != nil || string(got) != string(original) {
				t.Fatalf("existing cache changed: %q %v", got, err)
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(c.Path), ".asn-cache-*"))
			if len(matches) != 0 {
				t.Fatalf("temporary files remain: %v", matches)
			}
		})
	}
}
func TestPersistenceAtomicReplacementAndExpiredEntries(t *testing.T) {
	c := cfg()
	c.Persist = true
	c.Path = filepath.Join(t.TempDir(), "cache.json")
	now := time.Now()
	old := []Entry{{ASN: 1, Description: "expired", LookedUp: now.Add(-2 * time.Hour), Expires: now.Add(-time.Hour)}, {ASN: 2, Description: "valid", LookedUp: now.Add(-time.Minute), Expires: now.Add(time.Minute)}}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(c.Path, b, 0400); err != nil {
		t.Fatal(err)
	}
	s := New(c, nil)
	defer s.Close()
	if _, ok := s.Get(1); ok {
		t.Fatal("expired loaded")
	}
	if _, ok := s.Get(2); !ok {
		t.Fatal("valid entry missing")
	}
	s.mu.Lock()
	s.put(Entry{ASN: 3, Description: "new", LookedUp: now, Expires: now.Add(time.Hour)})
	s.dirty = true
	s.mu.Unlock()
	renamed := false
	real := s.fs.rename
	s.fs.rename = func(from, to string) error {
		before, err := os.ReadFile(to)
		if err != nil || string(before) != string(b) {
			t.Error("old file changed before rename")
		}
		data, err := os.ReadFile(from)
		if err != nil || !json.Valid(data) {
			t.Error("temporary file incomplete before rename")
		}
		renamed = true
		return real(from, to)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if !renamed {
		t.Fatal("rename not called")
	}
	data, _ := os.ReadFile(c.Path)
	if !json.Valid(data) || string(data) == string(b) {
		t.Fatal("replacement failed")
	}
	info, _ := os.Stat(c.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
}
func TestPersistenceMissingDirectory(t *testing.T) {
	c := cfg()
	c.Persist = true
	c.Path = filepath.Join(t.TempDir(), "missing", "cache.json")
	s := New(c, nil)
	now := time.Now()
	s.mu.Lock()
	s.put(Entry{ASN: 1, Description: "one", LookedUp: now, Expires: now.Add(time.Hour)})
	s.dirty = true
	s.mu.Unlock()
	if err := s.flush(); err == nil {
		t.Fatal("missing directory accepted")
	}
	if _, ok := s.Get(1); !ok {
		t.Fatal("in-memory cache lost")
	}
	s.Close()
}

func TestPersistenceReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	c := cfg()
	c.Persist = true
	c.Path = filepath.Join(dir, "cache.json")
	s := New(c, nil)
	defer s.Close()
	now := time.Now()
	s.mu.Lock()
	s.put(Entry{ASN: 1, Description: "one", LookedUp: now, Expires: now.Add(time.Hour)})
	s.dirty = true
	s.mu.Unlock()
	err := s.flush()
	if err == nil {
		t.Skip("process can write despite read-only permissions; injected permission failure is tested separately")
	}
	if _, ok := s.Get(1); !ok {
		t.Fatal("cache unavailable after write failure")
	}
}
