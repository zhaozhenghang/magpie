package provider

import (
	"context"
	"strings"
	"sync"
	"time"
)

// WeeklyQuotaFresh applies the Weekly Balance strategy's sample requirements.
func WeeklyQuotaFresh(windows []RoutingQuotaWindow, now time.Time, maxAge time.Duration) bool {
	const week = 7 * 24 * time.Hour
	var found bool
	for _, q := range windows {
		if q.ModelScoped || q.SpanMS != week.Milliseconds() {
			continue
		}
		start := q.ResetsAt - q.SpanMS
		at := now.UnixMilli()
		if found || q.ResetsAt <= at || start > at || q.ReadAt < start ||
			q.ReadAt > at+time.Minute.Milliseconds() || at-q.ReadAt > maxAge.Milliseconds() {
			return false
		}
		found = true
	}
	return found
}

var weeklyQuotaReads = struct {
	sync.Mutex
	active map[string]chan struct{}
	last   map[string]time.Time
}{active: map[string]chan struct{}{}, last: map[string]time.Time{}}

// PrepareRemoteWeek returns a shared read to wait on when the local copy
// of a remote's weekly quota is missing or stale. The read may outlast
// the request waiting for it, so a later turn can use the result.
func PrepareRemoteWeek(p Provider, model string, maxAge time.Duration) <-chan struct{} {
	card, _, ok := strings.Cut(model, "/")
	if !ok || card == "" || !p.IsRemoteMagpie() || p.Key == "" || p.Anthropic == "" {
		return nil
	}
	keyForCard := func(inner string) string { return strings.TrimSuffix(model, "/"+inner) }
	allowance, inner, known := RemoteAllowance(p, model, false)
	if known {
		if WeeklyQuotaFresh(allowance.QuotaWindows(inner), time.Now(), maxAge) {
			return nil
		}
		card = keyForCard(inner)
	} else if at := strings.LastIndex(model, "/"); at > len(card) {
		card = model[:at]
	}
	id := remoteCardsID(p)
	key := id + "\x00" + card
	reads := &weeklyQuotaReads
	reads.Lock()
	if pending := reads.active[key]; pending != nil {
		reads.Unlock()
		return pending
	}
	if time.Since(reads.last[key]) < remoteCardsFor {
		reads.Unlock()
		return nil
	}
	for k, at := range reads.last {
		if time.Since(at) >= remoteCardsFor {
			delete(reads.last, k)
		}
	}
	done := make(chan struct{})
	reads.active[key] = done
	reads.Unlock()
	goRead(func() {
		defer func() {
			allowance, inner, known := RemoteAllowance(p, model, false)
			fresh := known && WeeklyQuotaFresh(allowance.QuotaWindows(inner), time.Now(), maxAge)
			reads.Lock()
			delete(reads.active, key)
			if !fresh {
				reads.last[key] = time.Now()
			}
			close(done)
			reads.Unlock()
		}()
		<-askRemoteForRouting(p, id)
		remoteCardCache.Lock()
		cached := remoteCardCache.m[id]
		remoteCardCache.Unlock()
		if cached.err != "" || cached.held || readsAsked() {
			return
		}
		if allowance, inner, known := RemoteAllowance(p, model, false); known {
			if WeeklyQuotaFresh(allowance.QuotaWindows(inner), time.Now(), maxAge) {
				return
			}
			card = keyForCard(inner)
		}
		refreshRemote(context.Background(), p, card, "")
	})
	return done
}
