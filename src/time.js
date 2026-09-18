'use strict';

// 所有时间在存储与计算中统一使用 UTC 毫秒；对外展示才转换到条款声明的时区。
// 截止时刻按"条款时区的挂钟时间"定义：例如合同写 2026-03-31 17:00 Europe/London，
// 就必须按伦敦的挂钟（含夏令时）解释，再换算成 UTC，避免跨时区团队各算各的。

const MINUTE_MS = 60_000;
const DAY_MS = 86_400_000;

function utc(iso) {
  if (typeof iso === 'number') return iso;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) throw new Error(`无法解析时间: ${iso}`);
  return t;
}

function toIso(t) {
  return new Date(t).toISOString();
}

// 给定时区下某一挂钟日期/时间，返回 UTC 毫秒。
// wall 形如 '2026-03-31' 或 '2026-03-31T17:00'；秒与毫秒可选。
function zonedWallToUtc(wall, timeZone) {
  if (/Z|[+-]\d{2}:?\d{2}$/.test(wall)) {
    // 已带绝对偏移，直接按绝对时间处理（调用方本不该混用，这里做容错）
    return utc(wall);
  }
  const m = wall.match(/^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,3}))?)?)?$/);
  if (!m) throw new Error(`挂钟时间格式错误: ${wall}`);
  const year = +m[1], month = +m[2] - 1, day = +m[3];
  const hour = m[4] === undefined ? 0 : +m[4];
  const minute = m[5] === undefined ? 0 : +m[5];
  const second = m[6] === undefined ? 0 : +m[6];
  const msPart = m[7] === undefined ? 0 : +m[7].padEnd(3, '0');

  if (timeZone === 'UTC' || timeZone === undefined || timeZone === null) {
    return Date.UTC(year, month, day, hour, minute, second, msPart);
  }

  // Intl 往返法：用候选 UTC 时刻格式化为目标时区挂钟，按差值修正。
  // 夏令时跳变当天可能有不存在/重叠的挂钟，循环两次即可收敛到最近的合法时刻。
  let guess = Date.UTC(year, month, day, hour, minute, second, msPart);
  for (let i = 0; i < 3; i++) {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone, hour12: false,
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    }).formatToParts(new Date(guess));
    const get = (t) => +parts.find((p) => p.type === t).value;
    const asUtc = Date.UTC(get('year'), get('month') - 1, get('day'),
      get('hour') % 24, get('minute'), get('second'));
    const want = Date.UTC(year, month, day, hour, minute, second);
    const diff = want - asUtc;
    if (diff === 0) break;
    guess += diff;
  }
  return guess;
}

// 截止判定：now 与 deadline 都是 UTC 毫秒。
// 宽限期内不算逾期（宽限期按自然日 ×24h 处理，合同若按工作日定义需在条款层另行扩展）。
function deadlineStatus(now, deadlineUtc, graceDays = 0) {
  const graceEnd = deadlineUtc + graceDays * DAY_MS;
  if (now <= deadlineUtc) return 'ontime';
  if (now <= graceEnd) return 'within_grace';
  return 'late';
}

function isLate(now, deadlineUtc, graceDays = 0) {
  return deadlineStatus(now, deadlineUtc, graceDays) === 'late';
}

function startOfPeriodUtc(periodEndUtc, months) {
  const d = new Date(periodEndUtc);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() - months, 1);
}

// 观察期期初按"挂钟日历"定义：期末所在月往前推 (months-1) 个月的 1 号。
// 例：季度期，期末 2026-06-30 -> 期初 2026-04-01；期末 2026-03-31 -> 2026-01-01。
// 必须用挂钟日期而非 UTC，否则美洲时区期末挂钟当天在 UTC 已跨月，会把季度起点算错。
function startOfPeriodWall(periodEndWall, months) {
  const [y, m] = periodEndWall.split('-').map(Number);
  const d = new Date(Date.UTC(y, m - 1 - (months - 1), 1));
  return d.toISOString().slice(0, 10);
}

function addMonthsUtc(t, months) {
  const d = new Date(t);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + months, d.getUTCDate(),
    d.getUTCHours(), d.getUTCMinutes(), d.getUTCSeconds(), d.getUTCMilliseconds());
}

function minutesBetween(aIso, bIso) {
  return Math.round((utc(bIso) - utc(aIso)) / MINUTE_MS);
}

module.exports = {
  MINUTE_MS, DAY_MS,
  utc, toIso, zonedWallToUtc,
  deadlineStatus, isLate,
  startOfPeriodUtc, startOfPeriodWall, addMonthsUtc, minutesBetween,
};
