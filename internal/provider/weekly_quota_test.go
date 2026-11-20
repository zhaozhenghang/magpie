package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareRemoteWeekSharesRefresh(t *testing.T) {
	ForgetRemoteCardsForTest()
	t.Cleanup(ForgetRemoteCardsForTest)
	now := time.Now()
	reset := now.Add(24 * time.Hour)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var gets, posts atomic.Int32
	var fresh, replaced atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == RemoteCardsPath:
			gets.Add(1)
		case r.Method == http.MethodPost && r.URL.Path == RemoteRefreshPath && r.URL.Query().Get("provider") == "claude":
			if posts.Add(1) == 1 {
				close(started)
			}
			<-release
			fresh.Store(true)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		read := now.Add(-time.Hour)
		used, renew := 20.0, reset
		user := "original@example.invalid"
		if fresh.Load() {
			read = now.Add(-2 * time.Minute)
		}
		if replaced.Load() {
			read, used, renew = now, 9, reset.Add(24*time.Hour)
			user = "replacement@example.invalid"
		}
		cards := ToldRouting([]SubscriptionQuota{{Provider: "claude", User: user, ReadAt: &read,
			Windows: []QuotaWindow{{Name: "7 days", Span: 7 * 24 * time.Hour, Used: used, ResetsAt: &renew}},
		}})
		json.NewEncoder(w).Encode(map[string]any{"data": cards})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	p := Provider{ID: "weekly-peer", Preset: RemoteMagpiePreset, Key: server.URL, Anthropic: server.URL}
	const maxAge = 30 * time.Minute
	done := PrepareRemoteWeek(p, "claude/opus", maxAge)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stale card was not refreshed")
	}
	if joined := PrepareRemoteWeek(p, "claude/sonnet", maxAge); joined != done {
		t.Fatal("a refresh in progress was not shared with the same card")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the quota refresh did not finish")
	}
	if gets.Load() != 2 || posts.Load() != 1 {
		t.Fatalf("GETs: %d, refreshes: %d, want two GETs and one refresh", gets.Load(), posts.Load())
	}
	if a, model, ok := RemoteAllowance(p, "claude/opus", false); !ok || !WeeklyQuotaFresh(a.QuotaWindows(model), time.Now(), maxAge) {
		t.Fatalf("weekly allowance still stale after refresh: %+v", a)
	}
	ForgetRemoteCardsForTest()
	select {
	case <-PrepareRemoteWeek(p, "claude/opus", maxAge):
	case <-time.After(5 * time.Second):
		t.Fatal("a fresh cached card on the remote did not become ready")
	}
	if gets.Load() != 3 || posts.Load() != 1 {
		t.Fatalf("GETs: %d, refreshes: %d, a fresh remote card must not ask the vendor", gets.Load(), posts.Load())
	}
	replaced.Store(true)
	select {
	case <-PrepareRemoteWeek(p, "claude/opus", time.Minute):
	case <-time.After(5 * time.Second):
		t.Fatal("the same remote did not supply the replacement account's quota")
	}
	a, inner, ok := RemoteAllowance(p, "claude/opus", false)
	windows := a.QuotaWindows(inner)
	if !ok || len(windows) != 1 || windows[0].Used != 9 || windows[0].ResetsAt != reset.Add(24*time.Hour).UnixMilli() {
		t.Fatalf("replacement account windows: %+v", windows)
	}
}
