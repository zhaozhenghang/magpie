package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/middleware"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/settings"
)

func TestWeeklyBalancePluginOnRemoteGroup(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "")
	provider.ForgetRemoteCardsForTest()
	t.Cleanup(provider.ForgetRemoteCardsForTest)
	src, err := filepath.Abs("../../contrib/weekly-balance/weekly-balance.gateway.js")
	if err != nil {
		t.Fatal(err)
	}
	list, err := json.Marshal(plugin.List{Plugins: []plugin.Entry{{Spec: src}}})
	if err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(settings.Dir(), "plugins.json")
	if err := os.MkdirAll(filepath.Dir(listPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath, list, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(listPath)
		middleware.Reload()
		plugin.Settle()
	})
	middleware.Reload()
	stop := middleware.Points().(interface{ Serve() func() }).Serve()
	defer stop()
	const strategy = "weekly-balance/weekly-balance"
	if !routes.Served(strategy) {
		t.Fatal("the plugin strategy is unavailable:", middleware.States()[src])
	}

	const model = "claude/claude-sonnet-5-5"
	now := time.Now()
	read := now.Add(-time.Minute)
	shortReset := now.Add(time.Hour)
	var limitB atomic.Bool
	addRemote := func(id string, used, short float64, until time.Duration, limited *atomic.Bool) {
		t.Helper()
		reset := now.Add(until)
		cards := provider.ToldRouting([]provider.SubscriptionQuota{{
			Provider: "claude", Kind: "subscription", User: id + "@example.invalid",
			ReadAt: &read, Windows: []provider.QuotaWindow{
				{Name: "5 hours", Span: 5 * time.Hour, Used: short, ResetsAt: &shortReset},
				{Name: "7 days", Span: 7 * 24 * time.Hour, Used: used, ResetsAt: &reset},
			},
		}})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method + " " + r.URL.Path {
			case "GET " + provider.RemoteCardsPath:
				json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": cards})
			case "GET /v1/magpie/limit":
				http.NotFound(w, r)
			case "POST /v1/messages":
				if limited != nil && limited.Load() {
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Your quota resets in 3 hours."}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"test","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
					"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"served by `+id+`"}}`,
					"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
					"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
					"event: message_stop\ndata: "+`{"type":"message_stop"}`))
			default:
				t.Errorf("unexpected request to %s: %s %s", id, r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		if err := provider.Save(provider.Provider{ID: id, Name: id, Preset: provider.RemoteMagpiePreset,
			Key: "test-key", Chat: server.URL + "/v1", Models: []string{model}}); err != nil {
			t.Fatal(err)
		}
	}
	addRemote("remote-a", 50, 8, 36*time.Hour, nil)
	addRemote("remote-b", 24, 68, 72*time.Hour, &limitB)
	provider.RemoteCards(context.Background())
	for _, id := range []string{"remote-a", "remote-b"} {
		p, err := provider.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		a, inner, ok := provider.RemoteAllowance(*p, model, false)
		if !ok || len(a.QuotaWindows(inner)) != 2 {
			t.Fatalf("no dated %s allowance for %s: %v", id, inner, a.QuotaWindows(inner))
		}
	}
	if err := provider.SaveGroup(provider.Group{Name: "Weekly", Routing: strategy,
		Affinity: provider.AffinityTurn, Members: []string{"remote-a/" + model, "remote-b/" + model}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	ask := func(t *testing.T, session, request, want string) {
		t.Helper()
		code, body := postAs(t, s, session, request)
		if code != http.StatusOK || !strings.Contains(body, "served by "+want) {
			t.Fatalf("want %s, got %d %s, strategy state %+v", want, code, body, middleware.States()[src])
		}
	}
	const first = `{"model":"group/weekly","messages":[{"role":"user","content":"test"}]}`
	const tool = `{"model":"group/weekly","messages":[{"role":"user","content":"test"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"a.go"}]}`
	ask(t, "first", first, "remote-b")
	for _, w := range lastRoute(s).Order {
		if !slices.ContainsFunc(w.QuotaWindows, func(q provider.RoutingQuotaWindow) bool {
			return q.SpanMS == (7*24*time.Hour).Milliseconds() && q.ReadAt == read.UnixMilli() && q.ResetsAt > now.UnixMilli()
		}) {
			t.Fatalf("%s: missing weekly quota snapshot in routing trace: %+v", w.ID, w.QuotaWindows)
		}
	}
	last, err := provider.Find("remote-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want, why string
		idle            time.Duration
		minutes         int
		kept            bool
	}{
		{"within an hour", "remote-a", "turn", 59 * time.Minute, 60, true},
		{"at one hour", "remote-b", "turn-timeout", time.Hour, 60, false},
		{"overnight", "remote-b", "turn-timeout", 15 * time.Hour, 60, false},
		{"within configured limit", "remote-a", "turn", 19 * time.Minute, 20, true},
		{"at configured limit", "remote-b", "turn-timeout", 20 * time.Minute, 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefs := settings.Load()
			prefs.TurnAffinityMinutes = tc.minutes
			if err := settings.Save(prefs); err != nil {
				t.Fatal(err)
			}
			h := make(http.Header)
			h.Set("x-session-id", tc.name)
			key := stickKey("group/weekly", h, []byte(tool))
			answered(key, candidate{p: *last, model: model, rest: last.ID}, 1, Usage{CacheRead: 400_000})
			sticks.Lock()
			st := sticks.m[key]
			st.at = st.at.Add(-tc.idle)
			sticks.m[key] = st
			sticks.Unlock()
			answered(stickKey("group/other", h, []byte(tool)),
				candidate{p: *last, model: model, rest: last.ID}, 1, Usage{CacheRead: 500_000})

			ask(t, tc.name, tool, tc.want)
			if aff := lastRoute(s).Affinity; aff == nil || !aff.Within || aff.Kept != tc.kept || aff.Why != tc.why {
				t.Fatalf("affinity = %+v, want within=true kept=%v why=%s", aff, tc.kept, tc.why)
			}
		})
	}
	limitB.Store(true)
	ask(t, "second", first, "remote-a")
	if state := middleware.States()[src]; state.Orders < 2 || state.Failures != 0 {
		t.Fatalf("strategy state = %+v", state)
	}
}

func TestProviderTurnAffinityUsesGlobalTimeout(t *testing.T) {
	fresh(t)
	next := candidate{p: provider.Provider{ID: "first"}, rest: "first", model: "m"}
	last := candidate{p: provider.Provider{ID: "last"}, rest: "last", model: "m"}
	const scope = "provider/m"
	const tool = `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"a.go"}]}`
	in := http.Header{"X-Session-Id": {"direct-provider"}}
	body := []byte(tool)
	key := stickKey(scope, in, body)
	answered(key, last, 1, Usage{CacheRead: 2000})
	sticks.Lock()
	record := sticks.m[key]
	record.at = record.at.Add(-25 * time.Minute)
	sticks.m[key] = record
	sticks.Unlock()

	for _, tc := range []struct {
		minutes int
		want    string
		why     string
	}{{60, "last", "turn"}, {20, "first", "turn-timeout"}} {
		prefs := settings.Load()
		prefs.TurnAffinityMinutes = tc.minutes
		if err := settings.Save(prefs); err != nil {
			t.Fatal(err)
		}
		cs, _, aff, _ := affine(scope, provider.AffinityTurn, false, false, true, false, in, provider.Chat, body, body,
			[]candidate{next, last}, planned{order: []Weighed{{ID: next.rest}, {ID: last.rest}}})
		if cs[0].rest != tc.want || aff.Why != tc.why {
			t.Fatalf("%d minutes: first route %s with affinity %+v, want %s by %s", tc.minutes, cs[0].rest, aff, tc.want, tc.why)
		}
	}
}
