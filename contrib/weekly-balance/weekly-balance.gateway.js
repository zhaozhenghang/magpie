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
  // A one-point band keeps small sampling differences out of the sort.
  return Math.round(window.used - target * elapsed);
}

function shortUse(seat, now, maxAge) {
  let used = null;
  for (const window of seat.quotaWindows || []) {
    if (window.spanMs === 5 * hour && window.resetsAt > now && recent(window, now, maxAge)) {
      used = used === null ? window.used : Math.max(used, window.used);
    }
  }
  return used;
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
    short: shortUse(seat, now, maxAge),
    tokens: seat.tokens || 0,
  }));

  const available = order.filter(item => !item.unavailable);
  const compareWeek = available.length > 1 && available.every(item => item.weekly !== null);
  const compareShort = compareWeek && available.every(item => item.short !== null);
  order.sort((a, b) => {
    if (a.unavailable !== b.unavailable) return a.unavailable ? 1 : -1;
    if (a.unavailable || !compareWeek) return a.index - b.index;
    return a.weekly - b.weekly || (compareShort ? a.short - b.short : 0) ||
      a.tokens - b.tokens || a.index - b.index;
  });
  return order.map(item => item.index);
}

export const strategies = { "weekly-balance": weeklyBalance };
