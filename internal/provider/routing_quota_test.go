package provider

import (
	"slices"
	"testing"
	"time"
)

func TestRoutedAllowanceQuotaWindows(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	readAt := now.Add(-15 * time.Minute)
	week := 7 * 24 * time.Hour
	nextWeek := now.Add(3 * 24 * time.Hour)
	alreadyReset := now.Add(-time.Minute)
	q := SubscriptionQuota{ReadAt: &readAt, Windows: []QuotaWindow{
		{Span: 5 * time.Hour, Used: 81, ResetSecs: 3600},
		{Span: week, Used: 33, ResetsAt: &nextWeek},
		{Span: week, Used: 92, ResetsAt: &alreadyReset, Model: "opus"},
		{Span: week, Used: 91, Model: "sonnet"},
		{Span: week, Used: 99, Aside: true},
		{Span: week, Used: 99, Text: true},
	}}
	a, ok := routedAllowance("claude", "seat", q, now)
	if !ok {
		t.Fatal("allowance unavailable")
	}
	got := a.QuotaWindows("claude-OPUS")
	want := []RoutingQuotaWindow{
		{SpanMS: (5 * time.Hour).Milliseconds(), Used: 81, ResetsAt: readAt.Add(time.Hour).UnixMilli(), ReadAt: readAt.UnixMilli()},
		{SpanMS: week.Milliseconds(), Used: 33, ResetsAt: nextWeek.UnixMilli(), ReadAt: readAt.UnixMilli()},
		{SpanMS: week.Milliseconds(), Used: 92, ResetsAt: alreadyReset.UnixMilli(), ReadAt: readAt.UnixMilli(), ModelScoped: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("windows = %+v, want %+v", got, want)
	}
	if used, _ := a.For("claude-opus", now); used != 81 {
		t.Errorf("aggregate used = %v, want the unchanged 81%%", used)
	}
	q.ReadAt = nil
	a, _ = routedAllowance("claude", "seat", q, now)
	if got := a.QuotaWindows("claude-opus"); len(got) != 0 {
		t.Errorf("undated reading exported as windows: %+v", got)
	}
}
