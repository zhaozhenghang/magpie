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
	var fresh atomic.Bool
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
		if fresh.Load() {
			read = now
		}
		cards := ToldRouting([]SubscriptionQuota{{Provider: "claude", ReadAt: &read,
			Windows: []QuotaWindow{{Name: "7 days", Span: 7 * 24 * time.Hour, Used: 20, ResetsAt: &reset}},
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
}
