package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/routes"
	"github.com/yetone/magpie/internal/settings"
)

// A vendor quota read can take several seconds. A slow read can finish in
// the background for the next turn instead of holding every request up.
const weeklyQuotaWait = 6 * time.Second

// weeklyCacheHolds compares the selected seat to the last answerer after
// native routing has applied caps, access, fallbacks and protocol fit.
func weeklyCacheHolds(routing, scope string, from provider.Protocol, cs []candidate, order []Weighed, at int, last stick) bool {
	if routing != routes.WeeklyBalance || at <= 0 {
		return false
	}
	next, previous := order[0], order[at]
	c := cs[at]
	if next.Routing != routing || previous.Routing != routing ||
		next.WeeklyQuotaFresh == nil || previous.WeeklyQuotaFresh == nil ||
		next.Fallback != previous.Fallback || previous.Sunk != nil ||
		keyFit(cs[0].p, cs[0].model, from) != keyFit(c.p, c.model, from) ||
		c.who() != last.who || c.model != last.model || c.effort != last.effort ||
		!cacheHolds(last, c.p) {
		return false
	}
	strategy, ok := routes.Find(routing)
	if !ok || strategy.Order == nil {
		return false
	}
	seats := []routes.Seat{
		{Key: next.ID, Used: next.Used, Resting: next.Rest != nil, QuotaWindows: next.QuotaWindows},
		{Key: previous.ID, Used: previous.Used, Resting: previous.Rest != nil, QuotaWindows: previous.QuotaWindows, CacheWarm: true},
	}
	return routes.Apply(strategy, scope, seats)[0] == 1
}

func prepareWeeklyQuota(ctx context.Context, g provider.Group, members []provider.Member, scope string, header http.Header, from provider.Protocol, body, plain []byte) {
	if g.Routing != routes.WeeklyBalance || len(members) < 2 || ctx.Err() != nil {
		return
	}
	strategy, ok := routes.Find(g.Routing)
	if !ok || strategy.QuotaMaxAge <= 0 {
		return
	}
	if g.Affinity == provider.AffinityTurn {
		if _, within := turnOf(from, plain); within {
			key := stickKey(scope, header, body)
			sticks.Lock()
			last, had := stickOf(key)
			sticks.Unlock()
			if had && time.Since(last.at) < settings.Shared().TurnAffinityIdle() {
				return
			}
		}
	}
	var reads []<-chan struct{}
	for _, m := range members {
		if done := provider.PrepareRemoteWeek(m.Provider, m.Model, strategy.QuotaMaxAge); done != nil {
			reads = append(reads, done)
		}
	}
	if len(reads) == 0 {
		return
	}
	wait, cancel := context.WithTimeout(ctx, weeklyQuotaWait)
	defer cancel()
	for _, read := range reads {
		select {
		case <-read:
		case <-wait.Done():
			return
		}
	}
}
