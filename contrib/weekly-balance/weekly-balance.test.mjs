import assert from "node:assert/strict";
import test from "node:test";
import { strategies } from "./weekly-balance.gateway.js";

const hour = 3_600_000;
const week = 168 * hour;
const at = Date.parse("2026-11-19T18:30:00+08:00");
const route = (seats, options = {}) => strategies["weekly-balance"]("group/claude", seats, options);

function seat(now, weeklyUsed, weeklyReset, shortUsed, overrides = {}) {
  return {
    used: Math.max(weeklyUsed, shortUsed),
    quotaWindows: [
      { spanMs: 5 * hour, used: shortUsed, resetsAt: now + hour, readAt: now },
      { spanMs: week, used: weeklyUsed, resetsAt: weeklyReset, readAt: now },
    ],
    ...overrides,
  };
}

test("ranks by weekly progress with offset resets", t => {
  t.mock.method(Date, "now", () => at);
  const a = seat(at, 30, at + 120 * hour, 10);
  const b = seat(at, 40, at + 36 * hour, 65);
  assert.deepEqual(route([a, b]), [1, 0]);

  const c = seat(at, 40, at + 36 * hour, 10);
  b.tokens = 10_000;
  assert.deepEqual(route([b, c]), [0, 1]);
  assert.deepEqual(route([a, b], { weeklyTarget: 10 }), [0, 1]);
  b.resting = true;
  assert.deepEqual(route([b, a]), [1, 0]);
});

test("a small weekly lead beats a lower 5h reading without overriding a hard cap", t => {
  const now = Date.parse("2026-11-20T11:27:20+08:00");
  t.mock.method(Date, "now", () => now);
  const dmit = seat(now, 8, Date.parse("2026-11-27T02:00:00+08:00"), 63);
  const ntt = seat(now, 41, Date.parse("2026-11-24T01:00:00+08:00"), 61);
  assert.deepEqual(route([ntt, dmit]), [1, 0]);
  dmit.used = dmit.quotaWindows[0].used = 100;
  assert.deepEqual(route([ntt, dmit]), [0, 1]);
});

test("a missing or stale week falls back to recent work, then list order", t => {
  t.mock.method(Date, "now", () => at);
  const valid = seat(at, 12, at + 48 * hour, 10);
  const stale = seat(at, 65, at + 84 * hour, 10);
  stale.quotaWindows[1].readAt -= 31 * 60_000;
  assert.deepEqual(route([stale, valid]), [0, 1]);
  assert.deepEqual(route([stale, valid], { maxSampleAgeMinutes: 40 }), [1, 0]);
  assert.deepEqual(route([seat(at, 80, at, 20), valid]), [0, 1]);
  assert.deepEqual(route([seat(at, 0, at + week + hour, 20), valid]), [0, 1]);

  const scoped = seat(at, 70, at + 84 * hour, 10);
  scoped.quotaWindows[1].modelScoped = true;
  assert.deepEqual(route([scoped, valid]), [0, 1]);
  assert.deepEqual(route([{ used: 81 }, valid]), [0, 1]);
  const ambiguous = seat(at, 70, at + 84 * hour, 10);
  ambiguous.quotaWindows.push(ambiguous.quotaWindows[1]);
  assert.deepEqual(route([ambiguous, valid]), [0, 1]);
  assert.deepEqual(route([{ used: 100 }, valid]), [1, 0]);
  valid.tokens = 20_000;
  assert.deepEqual(route([valid, stale]), [1, 0]);
});

test("the first new sample after a reset starts the new cycle across year boundaries", t => {
  const reset = Date.parse("2027-01-01T02:00:00+08:00");
  let now = reset - 60_000;
  t.mock.method(Date, "now", () => now);
  let a = seat(now, 75, reset, 20);
  const b = seat(now, 18, reset + 80 * hour, 18);
  assert.deepEqual(route([a, b]), [1, 0]);
  now = reset + hour;
  assert.deepEqual(route([a, b]), [0, 1]);
  a = seat(now, 1, reset + week, 20);
  b.quotaWindows.forEach(window => { window.readAt = now; });
  assert.deepEqual(route([a, b]), [1, 0]);
});

test("six weeks of staggered resets with variable work and bursts", t => {
  const start = Date.parse("2026-11-17T01:00:00+08:00");
  let now = start;
  t.mock.method(Date, "now", () => now);
  const accounts = [
    { reset: start + week, weekUsed: 0, samples: [], short: [] },
    { reset: Date.parse("2026-11-20T02:00:00+08:00"), weekUsed: 42, samples: [], short: [] },
  ];
  const round = 20 * 60_000;
  let seed = 7207;
  const random = () => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    return seed / 2 ** 32;
  };
  const weights = Array.from({ length: 6 * week / round }, (_, index) =>
    0.2 + random() * 1.6 + (index % 137 < 6 ? 1.5 : 0));
  const unit = 6 * 150 / weights.reduce((sum, weight) => sum + weight, 0);
  let refusals = 0;

  for (const weight of weights) {
    const use = unit * weight;
    const ready = [];
    for (const account of accounts) {
      if (now >= account.reset) {
        account.samples.push(account.weekUsed);
        account.weekUsed = 0;
        account.reset += week;
      }
      account.short = account.short.filter(record => now - record.at < 5 * hour);
      account.shortUsed = account.short.reduce((sum, record) => sum + record.use, 0);
      if (account.weekUsed < 80 && account.shortUsed < 80) ready.push(account);
    }
    if (!ready.length) {
      refusals++;
    } else {
      const ranked = route(ready.map(account =>
        seat(now, account.weekUsed, account.reset, account.shortUsed)));
      const chosen = ready[ranked[0]];
      assert.ok(chosen.weekUsed < 80 && chosen.shortUsed < 80);
      chosen.weekUsed += use;
      chosen.short.push({ at: now, use: 6 * use });
    }
    now += round;
  }
  assert.equal(refusals, 0);
  for (const account of accounts) {
    for (const used of account.samples.slice(1)) assert.ok(Math.abs(used - 75) <= 5, used);
  }
});
