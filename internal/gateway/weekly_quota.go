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
