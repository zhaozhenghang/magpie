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
	"sync"
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
	type remoteState struct {
		limited, stale atomic.Bool
		refreshes      atomic.Int32
		weekly         atomic.Int32
	}
	addRemote := func(id string, used, short float64, until time.Duration) *remoteState {
		t.Helper()
		state := &remoteState{}
		state.weekly.Store(int32(used))
		reset := now.Add(until)
		cards := func() []provider.SubscriptionQuota {
			at := read
			if state.stale.Load() {
				at = now.Add(-40 * time.Minute)
			}
			return provider.ToldRouting([]provider.SubscriptionQuota{{
				Provider: "claude", Kind: "subscription", User: id + "@example.invalid",
				ReadAt: &at, Windows: []provider.QuotaWindow{
					{Name: "5 hours", Span: 5 * time.Hour, Used: short, ResetsAt: &shortReset},
					{Name: "7 days", Span: 7 * 24 * time.Hour, Used: float64(state.weekly.Load()), ResetsAt: &reset},
				},
			}})
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method + " " + r.URL.Path {
			case "GET " + provider.RemoteCardsPath:
				json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": cards()})
			case "POST " + provider.RemoteRefreshPath:
				if r.URL.Query().Get("provider") != "claude" {
					t.Errorf("unexpected quota refresh on %s", id)
				}
				state.refreshes.Add(1)
				state.stale.Store(false)
				json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": cards()})
			case "GET /v1/magpie/limit":
				http.NotFound(w, r)
			case "POST /v1/messages":
				if state.limited.Load() {
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
		return state
	}
	a := addRemote("remote-a", 50, 8, 36*time.Hour)
	b := addRemote("remote-b", 24, 68, 72*time.Hour)
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
	group := provider.Group{Name: "Weekly", Routing: strategy,
		Affinity: provider.AffinityTurn, Members: []string{"remote-a/" + model, "remote-b/" + model}}
	if err := provider.SaveGroup(group); err != nil {
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
	t.Run("weekly cache preference across turns", func(t *testing.T) {
		a.weekly.Store(41) // its week is 0.9 points ahead of the target relative to remote-b
		provider.ForgetRemoteCardsForTest()
		provider.RemoteCards(context.Background())
		for _, tc := range []struct {
			name, want, why string
			idle            time.Duration
			usage           Usage
			minutes         int
		}{
			{"warm cache and close weekly progress", "remote-a", "cache", time.Minute, Usage{CacheRead: 400_000}, 60},
			{"small cache", "remote-b", "new-turn", time.Minute, Usage{CacheRead: 20}, 60},
			{"cache expired", "remote-b", "new-turn", 6 * time.Minute, Usage{CacheRead: 400_000}, 60},
			{"global idle limit expires first", "remote-b", "new-turn", 2 * time.Minute, Usage{CacheRead: 400_000}, 1},
		} {
			prefs := settings.Load()
			prefs.TurnAffinityMinutes = tc.minutes
			if err := settings.Save(prefs); err != nil {
				t.Fatal(err)
			}
			h := make(http.Header)
			h.Set("x-session-id", tc.name)
			key := stickKey("group/weekly", h, []byte(first))
			answered(key, candidate{p: *last, model: model, rest: last.ID}, 1, tc.usage)
			sticks.Lock()
			st := sticks.m[key]
			st.at = st.at.Add(-tc.idle)
			sticks.m[key] = st
			sticks.Unlock()
			ask(t, tc.name, first, tc.want)
			if aff := lastRoute(s).Affinity; aff == nil || aff.Within || aff.Why != tc.why || aff.Kept != (tc.why == "cache") {
				t.Fatalf("%s: affinity = %+v, want %s", tc.name, aff, tc.why)
			}
		}
		ask(t, "new session with close weekly progress", first, "remote-b")

		a.weekly.Store(50)
		provider.ForgetRemoteCardsForTest()
		provider.RemoteCards(context.Background())
		h := make(http.Header)
		h.Set("x-session-id", "large weekly gap")
		key := stickKey("group/weekly", h, []byte(first))
		answered(key, candidate{p: *last, model: model, rest: last.ID}, 1, Usage{CacheRead: 400_000})
		ask(t, "large weekly gap", first, "remote-b")
		if aff := lastRoute(s).Affinity; aff == nil || aff.Why != "new-turn" || aff.Kept {
			t.Fatalf("large weekly gap: affinity = %+v", aff)
		}
	})
	b.stale.Store(true)
	provider.ForgetRemoteCardsForTest()
	provider.RemoteCards(context.Background())
	ask(t, "refresh stale seat", first, "remote-b")
	if n := b.refreshes.Load(); n != 1 {
		t.Fatalf("got %d active quota refreshes, want one", n)
	}
	c := addRemote("added-peer", 10, 82, 60*time.Hour)
	c.stale.Store(true)
	group.Members = append(group.Members, "added-peer/"+model)
	if err := provider.SaveGroup(group); err != nil {
		t.Fatal(err)
	}
	ask(t, "third member", first, "added-peer")
	if n := c.refreshes.Load(); n != 1 || a.refreshes.Load() != 0 || b.refreshes.Load() != 1 {
		t.Fatalf("refresh counts after adding a stale third member: %d, %d, %d", a.refreshes.Load(), b.refreshes.Load(), n)
	}
	if order := lastRoute(s).Order; len(order) != 3 || order[0].Provider != "added-peer" {
		t.Fatalf("unexpected order with a third member: %+v", order)
	}
	t.Run("three accounts under one provider", func(t *testing.T) {
		const alias = "claude-local"
		pool := provider.Provider{ID: alias, Routing: strategy}
		known := map[string]provider.Allowance{}
		var candidates []candidate
		for _, id := range []string{"remote-a", "remote-b", "added-peer"} {
			peer, err := provider.Find(id)
			if err != nil {
				t.Fatal(err)
			}
			a, _, ok := provider.RemoteAllowance(*peer, model, false)
			if !ok {
				t.Fatalf("no allowance for %s", id)
			}
			user := id + "@example.invalid"
			known[user] = a
			acct := pool
			acct.Account = &provider.Account{Agent: "claude", User: user}
			candidates = append(candidates, candidate{p: acct, rest: user, model: "claude-sonnet-5-5"})
		}
		old := allowances
		allowances = func(string) map[string]provider.Allowance { return known }
		t.Cleanup(func() { allowances = old })
		ordered, _ := weighRouted(pool, candidates, "claude-sonnet-5-5", provider.Anthropic)
		if len(ordered) != 3 || ordered[0].p.Account.User != "added-peer@example.invalid" {
			t.Fatalf("unexpected local account order: %+v", ordered)
		}
	})
	group.Members = group.Members[:2]
	if err := provider.SaveGroup(group); err != nil {
		t.Fatal(err)
	}
	b.limited.Store(true)
	ask(t, "member removed and remaining member limited", first, "remote-a")
	if state := middleware.States()[src]; state.Orders < 2 || state.Failures != 0 {
		t.Fatalf("strategy state = %+v", state)
	}
}

func TestWeeklyQuotaWaitHonorsRequestDeadline(t *testing.T) {
	fresh(t)
	provider.ForgetRemoteCardsForTest()
	t.Cleanup(provider.ForgetRemoteCardsForTest)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == provider.RemoteRefreshPath {
			close(entered)
			<-release
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(peer.Close)
	t.Cleanup(unblock)
	const age = 30 * time.Minute
	const model = "claude/opus"
	remote := provider.Provider{ID: "slow-peer", Preset: provider.RemoteMagpiePreset, Key: peer.URL, Anthropic: peer.URL}
	pending := provider.PrepareRemoteWeek(remote, model, age)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the remote refresh did not start")
	}
	t.Cleanup(routes.Register(routes.WeeklyBalance, routes.Strategy{ByQuota: true, QuotaMaxAge: age}))
	group := provider.Group{ID: "weekly", Routing: routes.WeeklyBalance}
	members := []provider.Member{{Provider: remote, Model: model}, {Model: "local"}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	prepareWeeklyQuota(ctx, group, members, "group/weekly", nil, provider.Chat, nil, nil)
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("the caller didn't wait for the pending read: %v", ctx.Err())
	}
	select {
	case <-pending:
		t.Fatal("the caller canceled the shared refresh")
	default:
	}
	unblock()
	select {
	case <-pending:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending read did not finish")
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
