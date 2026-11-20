const hour = 60 * 60 * 1000;
const week = 7 * 24 * hour;

function option(value, fallback, minimum, maximum) {
  return Number.isFinite(value) && value >= minimum && value <= maximum ? value : fallback;
}

function recent(window, now, maxAge) {
  return Number.isFinite(window.readAt) && window.readAt <= now + 60_000 &&
    now - window.readAt <= maxAge && Number.isFinite(window.used) &&
    window.used >= 0 && window.used <= 100;
}

function weeklyProgress(seat, now, maxAge, target) {
  const windows = (seat.quotaWindows || []).filter(window =>
    window.spanMs === week && !window.modelScoped);
  if (windows.length !== 1) return null;
  const window = windows[0];
  const start = window.resetsAt - window.spanMs;
  if (!recent(window, now, maxAge) || !Number.isFinite(start) ||
      window.resetsAt <= now || start > now || window.readAt < start) return null;
  const elapsed = (now - start) / window.spanMs;
  return window.used - target * elapsed;
}

function weeklyBalance(_pool, seats, options) {
  const now = Date.now();
  const target = option(options?.weeklyTarget, 75, 1, 100);
  const maxAge = option(options?.maxSampleAgeMinutes, 30, 1, 120) * 60_000;
  const order = seats.map((seat, index) => ({
    index,
    // Core and the serving gateway remain responsible for all hard caps.
    unavailable: seat.resting || seat.used >= 100,
    weekly: weeklyProgress(seat, now, maxAge, target),
    tokens: seat.tokens || 0,
  }));

  const compare = (a, b) => {
    if (a.unavailable !== b.unavailable) return a.unavailable ? 1 : -1;
    if (a.unavailable) return 0;
    if (a.weekly === null && b.weekly === null) return a.tokens - b.tokens;
    if (a.weekly === null || b.weekly === null) return a.weekly === null ? 1 : -1;
    return a.weekly - b.weekly;
  };
  order.sort((a, b) => compare(a, b) || a.index - b.index);
  const priority = seats.map(() => 0);
  let rank = 0;
  for (let i = 0; i < order.length && !order[i].unavailable; i++) {
    if (i === 0 || compare(order[i - 1], order[i]) !== 0) rank++;
    priority[order[i].index] = rank;
  }
  return { order: order.map(item => item.index), priority };
}

export const strategies = { "weekly-balance": weeklyBalance };
