'use strict';

// 金额一律以"最小记账单位"整数（BigInt）存储，杜绝浮点误差；汇率以 1e8 定点数表示。
// 汇率口径：rate = 1 单位报告币种可兑换多少单位源币种（major 对 major，如 EURUSD 即 1 EUR = rate USD 时，
// 报告币为 EUR、源币为 USD 的汇率写法在领域层由 ingestion 明确方向，见 ingestRate 注释）。

const RATE_SCALE = 100_000_000n; // 1e8

const SCALE = new Map([
  // 零小数位币种
  ['JPY', 0], ['KRW', 0], ['VND', 0], ['CLP', 0], ['ISK', 0],
]);

function decimals(ccy) {
  return SCALE.has(ccy) ? SCALE.get(ccy) : 2;
}

function minorFactor(ccy) {
  return 10n ** BigInt(decimals(ccy));
}

// '1234.56' USD -> 123456n；'1000' JPY -> 1000n
function toMinor(major, ccy) {
  if (typeof major === 'bigint') return major;
  const s = String(major).trim();
  if (!/^-?\d+(\.\d+)?$/.test(s)) throw new Error(`金额格式错误: ${major}`);
  const neg = s.startsWith('-');
  const [intPart, fracRaw = ''] = s.replace(/^-/, '').split('.');
  const d = decimals(ccy);
  let frac = fracRaw.slice(0, d).padEnd(d, '0');
  if (fracRaw.length > d && /[1-9]/.test(fracRaw.slice(d))) {
    throw new Error(`金额 ${major} 超过币种 ${ccy} 的记账精度（${d} 位小数）`);
  }
  const v = BigInt(intPart) * minorFactor(ccy) + BigInt(frac || '0');
  return neg ? -v : v;
}

function toMajor(minor, ccy) {
  const f = minorFactor(ccy);
  const neg = minor < 0n;
  const abs = neg ? -minor : minor;
  const intPart = abs / f;
  const rem = abs % f;
  const d = decimals(ccy);
  let s = intPart.toString();
  if (d > 0) s += '.' + rem.toString().padStart(d, '0');
  return (neg ? '-' : '') + s;
}

// 汇率定点化。输入字符串避免浮点。允许小于 1 的汇率（如 1 USD = 0.79 GBP）。
function rateToScaled(rateMajorString) {
  const s = String(rateMajorString).trim();
  const m = s.match(/^(\d+)(?:\.(\d+))?$/);
  if (!m) throw new Error(`汇率格式错误: ${rateMajorString}`);
  const intPart = BigInt(m[1]);
  const frac = (m[2] || '').slice(0, 8).padEnd(8, '0');
  if ((m[2] || '').length > 8) throw new Error(`汇率精度超过 8 位: ${rateMajorString}`);
  const scaled = intPart * RATE_SCALE + BigInt(frac);
  if (scaled <= 0n) throw new Error(`汇率必须为正: ${rateMajorString}`);
  return scaled;
}

// 通用非负定点数（阈值等），同样 8 位小数、允许 0。
function fixedToScaled(majorString) {
  const s = String(majorString).trim();
  const m = s.match(/^(\d+)(?:\.(\d+))?$/);
  if (!m) throw new Error(`数值格式错误: ${majorString}`);
  if ((m[2] || '').length > 8) throw new Error(`数值精度超过 8 位: ${majorString}`);
  return BigInt(m[1]) * RATE_SCALE + BigInt((m[2] || '').padEnd(8, '0'));
}

// 口径换算：rate 含义 = 每 1 单位源币可兑换多少报告币（major 对 major）。
// 例如源币 GBP、报告币 USD，1 GBP = 1.25 USD 时写 '1.25000000'；SGD '0.74000000'。
// convertedMinor = srcMinor * rate，按最小记账单位四舍五入（half-up）。
// 返回 converted、舍入余量（源币最小单位，BigInt）与是否发生舍入，供审计解释引用。
function convert(srcMinor, srcCcy, reportingCcy, rateScaled) {
  if (srcCcy === reportingCcy) {
    return { converted: srcMinor, residual: 0n, rounded: false };
  }
  if (!rateScaled || rateScaled <= 0n) throw new Error(`缺少 ${srcCcy}->${reportingCcy} 的有效汇率`);
  const fSrc = minorFactor(srcCcy);
  const fDst = minorFactor(reportingCcy);
  const neg = srcMinor < 0n;
  const num = (neg ? -srcMinor : srcMinor) * rateScaled * fDst;
  const den = fSrc * RATE_SCALE;
  const q = num / den;
  const r = num % den;
  const halfUp = r * 2n >= den ? 1n : 0n;
  const roundedMinor = q + halfUp;
  // 回算舍入余量（源币最小单位）：rounded 按同汇率换回源币后与原值的差，仅供审计解释。
  const backSrc = (roundedMinor * fSrc * RATE_SCALE) / (fDst * rateScaled);
  const residualSrc = (neg ? -srcMinor : srcMinor) - backSrc;
  return {
    converted: neg ? -roundedMinor : roundedMinor,
    residual: neg ? -residualSrc : residualSrc,
    rounded: r !== 0n,
  };
}

module.exports = { RATE_SCALE, decimals, toMinor, toMajor, rateToScaled, fixedToScaled, convert };
