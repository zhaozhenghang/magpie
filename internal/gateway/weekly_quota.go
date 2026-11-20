package gateway

import (
	"context"
	"net/http"
	"slices"
	"strings"
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
	var reads []<-chan struct{}
	for _, m := range members {
		if done := provider.PrepareRemoteWeek(m.Provider, m.Model, strategy.QuotaMaxAge); done != nil {
			reads = append(reads, done)
		}
	}
	// Refresh in the background when native affinity could keep the route.
	// A cap or rule may still make it move; that request uses the snapshots
	// already available without holding an in-progress conversation up.
	if len(reads) == 0 || strings.TrimSpace(header.Get(AccountHeader)) != "" ||
		weeklyQuotaMayKeep(g.Affinity, members, scope, header, from, body, plain) {
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

// weeklyQuotaMayKeep only controls whether quota I/O can delay this request.
// The native affinity path still makes the routing decision.
func weeklyQuotaMayKeep(mode string, members []provider.Member, scope string, header http.Header, from provider.Protocol, body, plain []byte) bool {
	if mode == provider.AffinityOff {
		return false
	}
	key := stickKey(scope, header, body)
	sticks.Lock()
	last, had := stickOf(key)
	sticks.Unlock()
	idle := time.Since(last.at)
	if !had || idle > stickKeep {
		return false
	}
	switch mode {
	case provider.AffinitySession, provider.AffinityStrict:
		return true
	case provider.AffinityTurn:
		_, within := turnOf(from, plain)
		return within && idle < settings.Shared().TurnAffinityIdle()
	default:
		_, within := turnOf(from, plain)
		return within || slices.ContainsFunc(members, func(m provider.Member) bool { return cacheHolds(last, m.Provider) })
	}
}
