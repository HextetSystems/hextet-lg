package ptr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string) ([]string, error)

func (f resolverFunc) LookupAddr(c context.Context, s string) ([]string, error) { return f(c, s) }
func TestAddress(t *testing.T) {
	for _, tc := range []struct{ s, want string }{{"192.0.2.1", "192.0.2.1"}, {"2001:db8::1", "2001:db8::1"}, {"::ffff:192.0.2.1", "192.0.2.1"}, {"10.0.0.1", "10.0.0.1"}, {"fd00::1", "fd00::1"}, {"fe80::1", ""}, {"fe80::1%eth0", ""}, {"::", ""}, {"224.0.0.1", ""}, {"127.0.0.1", ""}, {"???", ""}, {"1.1.1.1:53", ""}, {"1.1.1.1/32", ""}, {"example.org", ""}} {
		a, ok := Address(tc.s)
		if ok != (tc.want != "") || ok && a.String() != tc.want {
			t.Errorf("%q: %v %v", tc.s, a, ok)
		}
	}
}
func TestName(t *testing.T) {
	n, ok := Name([]string{"z.example.", "Router.Example.", "a.example.", "<bad>"})
	if !ok || n != "a.example" {
		t.Fatal(n, ok)
	}
	for _, s := range []string{"", "bad..example", "-bad.example", "bad.example..", "<x>"} {
		if _, ok := Name([]string{s}); ok {
			t.Fatal(s)
		}
	}
}
func TestLookupCacheAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name             string
		records          []string
		err              error
		negative, cached bool
	}{{"positive", []string{"Router.Example."}, nil, false, true}, {"nodata", nil, nil, true, true}, {"nxdomain", nil, &net.DNSError{IsNotFound: true}, true, true}, {"servfail", nil, &net.DNSError{IsTemporary: true}, false, false}, {"timeout", nil, context.DeadlineExceeded, false, false}, {"canceled", nil, context.Canceled, false, false}, {"malformed", []string{"<bad>"}, nil, false, false}, {"failure", nil, errors.New("failure"), false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			s := New(Config{Timeout: time.Second, PositiveTTL: 50 * time.Millisecond, NegativeTTL: 100 * time.Millisecond, MaxConcurrent: 1, MaxEntries: 2}, resolverFunc(func(context.Context, string) ([]string, error) { close(done); return tc.records, tc.err }))
			defer s.Close()
			a, _ := Address("192.0.2.1")
			if !s.Schedule(a) || s.Schedule(a) {
				t.Fatal("dedup")
			}
			<-done
			deadline := time.Now().Add(time.Second)
			for {
				s.mu.Lock()
				pending := s.pending[a]
				s.mu.Unlock()
				if !pending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("pending")
				}
				time.Sleep(time.Millisecond)
			}
			e, ok := s.Get(a)
			if ok != tc.cached || ok && e.Negative != tc.negative {
				t.Fatalf("%+v %v", e, ok)
			}
			if ok && !tc.negative && e.Name != "Router.Example" {
				t.Fatal(e)
			}
			if !ok && s.Schedule(a) {
				t.Fatal("transient retry not delayed")
			}
		})
	}
}
func TestEvictionAndExpiry(t *testing.T) {
	s := New(Config{Timeout: time.Second, PositiveTTL: 10 * time.Millisecond, NegativeTTL: time.Second, MaxConcurrent: 1, MaxEntries: 1}, resolverFunc(func(context.Context, string) ([]string, error) { return []string{"router.example."}, nil }))
	defer s.Close()
	a, _ := Address("192.0.2.1")
	b, _ := Address("2001:db8::1")
	for _, ip := range []string{a.String(), b.String()} {
		addr, _ := Address(ip)
		s.Schedule(addr)
		until := time.Now().Add(time.Second)
		for {
			if _, ok := s.Get(addr); ok {
				break
			}
			if time.Now().After(until) {
				t.Fatal("lookup")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if _, ok := s.Get(a); ok {
		t.Fatal("not evicted")
	}
	time.Sleep(15 * time.Millisecond)
	if _, ok := s.Get(b); ok {
		t.Fatal("not expired")
	}
}

func TestQueueBoundAndShutdown(t *testing.T) {
	started := make(chan struct{}, 1)
	s := New(Config{Timeout: time.Minute, PositiveTTL: time.Hour, NegativeTTL: time.Hour, MaxConcurrent: 1, MaxEntries: 2}, resolverFunc(func(ctx context.Context, _ string) ([]string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	a, _ := Address("192.0.2.1")
	if !s.Schedule(a) {
		t.Fatal("initial schedule")
	}
	<-started
	for i := 2; i <= 17; i++ {
		ip, _ := Address(fmt.Sprintf("192.0.2.%d", i))
		if !s.Schedule(ip) {
			t.Fatalf("queue entry %d", i)
		}
	}
	extra, _ := Address("192.0.2.18")
	if s.Schedule(extra) {
		t.Fatal("unbounded queue")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel resolver")
	}
}
func TestNegativeExpiry(t *testing.T) {
	s := New(Config{Timeout: time.Second, PositiveTTL: time.Hour, NegativeTTL: 15 * time.Millisecond, MaxConcurrent: 1, MaxEntries: 1}, resolverFunc(func(context.Context, string) ([]string, error) { return nil, &net.DNSError{IsNotFound: true} }))
	defer s.Close()
	a, _ := Address("2001:db8::1")
	s.Schedule(a)
	deadline := time.Now().Add(time.Second)
	for {
		if e, ok := s.Get(a); ok {
			if !e.Negative {
				t.Fatal(e)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lookup")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := s.Get(a); ok {
		t.Fatal("negative entry did not expire")
	}
}
