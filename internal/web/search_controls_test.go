package web

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HextetSystems/hextet-lg/internal/config"
)

func TestM42SearchEnforcementAndForm(t *testing.T) {
	var hits atomic.Int32
	cfg, done := testConfig(defaults(), backendSpec{"a", "A", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/rib" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"rib":[{"prefix":"192.0.2.0/24","communities":["64500:1"],"extended_communities":["rt 64500:1"],"large_communities":["64500:1:2"]}]}`)
	}})
	defer done()
	no := false
	cfg.Features = config.Features{Communities: &no, LargeCommunities: &no, ExtendedCommunities: &no}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	handler := h.Routes()
	for _, query := range []string{"type=community&q=64500%3A1", "community=64500%3A1", "type=large-community&q=64500%3A1%3A2", "as=64500&large-community=64500%3A1%3A2", "type=ext-community&q=rt+64500%3A1", "as=64500&ext-community=rt+64500%3A1", "rib=in&as=1", "rib=out&as=1", "rib=local&as=1", "endpoint=rib-in&as=1", "endpoint=rib-out&as=1"} {
		for _, hx := range []bool{false, true} {
			var rr = get(handler, "/?"+query)
			if hx {
				rr = get(handler, "/?"+query, "HX-Request", "true")
			}
			if rr.Code != 400 {
				t.Errorf("%s hx=%v: %d", query, hx, rr.Code)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid search reached backend")
	}
	body := get(handler, "/").Body.String()
	for _, hidden := range []string{`name="endpoint"`, `name="community"`, `name="large-community"`, `name="ext-community"`, `value="large-community"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("unexpected %s", hidden)
		}
	}
	rr := get(handler, "/?type=prefix&q=192.0.2.0%2F24&backend=a")
	body = rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "64500:1:2") || !strings.Contains(body, "rt 64500:1") || !strings.Contains(body, "64500:1") {
		t.Fatal("community attributes missing")
	}
	if !strings.Contains(body, `<details class="advanced" id="advanced" >`) {
		t.Fatal("successful search should collapse")
	}
	rr = get(handler, "/?as=64500&af=ipv6&best=1&backend=a")
	body = rr.Body.String()
	for _, want := range []string{"(3 active)", `value="64500"`, `name="best" value="1" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	rr = get(handler, "/?prefix=bad&best=1")
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), `id="advanced" open`) || !strings.Contains(rr.Body.String(), `value="bad"`) {
		t.Fatal("validation state not preserved")
	}
}

func TestM42PrefixModesAndHistory(t *testing.T) {
	var seen string
	h := newTestHandler(t, defaults(), backendSpec{"a", "A", func(w http.ResponseWriter, r *http.Request) { seen = r.URL.RawQuery; fmt.Fprint(w, `{"rib":[]}`) }})
	for _, tc := range []struct{ query, upstream, canonical string }{
		{"type=prefix&q=8.8.8.0%2F24", "prefix=8.8.8.0%2F24", "type=prefix&q=8.8.8.0%2F24"},
		{"type=prefix&q=8.8.8.0%2F24&all=1", "all=1&prefix=8.8.8.0%2F24", "type=prefix&q=8.8.8.0%2F24&all=1"},
		{"type=prefix&q=2001%3Adb8%3A%3A%2F32&or-shorter=1", "or-shorter=1&prefix=2001%3Adb8%3A%3A%2F32", "type=prefix&q=2001%3Adb8%3A%3A%2F32&or-shorter=1"},
		{"type=prefix&q=8.8.8.8", "prefix=8.8.8.8", "type=prefix&q=8.8.8.8"},
		{"type=prefix&q=2001%3Adb8%3A%3A1", "prefix=2001%3Adb8%3A%3A1", "type=prefix&q=2001%3Adb8%3A%3A1"},
	} {
		rr := get(h, "/?"+tc.query+"&backend=a&view=table", "HX-Request", "true")
		if rr.Code != 200 || seen != tc.upstream || rr.Header().Get("HX-Push-Url") != "/?"+tc.canonical+"&backend=a&view=table" {
			t.Errorf("%s: %d %s %s", tc.query, rr.Code, seen, rr.Header().Get("HX-Push-Url"))
		}
	}
	for _, query := range []string{"prefix=8.8.8.0%2F24&all=1", "type=prefix&q=8.8.8.0%2F24&all=1"} {
		rr := get(h, "/?"+query+"&backend=a")
		if !strings.Contains(rr.Body.String(), "(1 active)") {
			t.Errorf("canonical filter count: %s", query)
		}
	}
	for _, q := range []string{"prefix=8.8.8.8&all=1", "prefix=2001%3Adb8%3A%3A1&or-shorter=1", "prefix=8.8.8.0%2F24&all=1&or-shorter=1"} {
		if rr := get(h, "/?"+q); rr.Code != 400 {
			t.Errorf("%s: %d", q, rr.Code)
		}
	}
}
