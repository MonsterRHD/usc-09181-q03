'use strict';

const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const M = require('../src/money');
const T = require('../src/time');

describe('金额与币种口径', () => {
  test('最小记账单位与零小数币种', () => {
    assert.equal(M.toMinor('1234.56', 'USD'), 123456n);
    assert.equal(M.toMinor('1000', 'JPY'), 1000n);
    assert.equal(M.toMajor(123456n, 'USD'), '1234.56');
    assert.equal(M.toMajor(1000n, 'JPY'), '1000');
    assert.equal(M.toMajor(5n, 'USD'), '0.05');
    assert.throws(() => M.toMinor('1.001', 'USD'), /记账精度/);
  });

  test('汇率换算为乘法并 half-up 舍入，残差可解释', () => {
    // 1 GBP = 1.25 USD：£816,000 -> $1,020,000，无舍入
    const rate = M.rateToScaled('1.25000000');
    const r1 = M.convert(M.toMinor('816000', 'GBP'), 'GBP', 'USD', rate);
    assert.equal(r1.converted, 102000000n);
    assert.equal(r1.rounded, false);

    // JPY（0 位小数）1 JPY = 0.0074 USD 之类的比例：验证小数币种混合舍入
    const r2 = M.convert(1000n, 'JPY', 'USD', M.rateToScaled('0.00740000'));
    // 1000 * .0074 = 7.40 USD -> 740 cents
    assert.equal(r2.converted, 740n);

    // 除不尽时 half-up：1 SGD = 1/3 USD 概念（0.33333333），10 SGD -> 3.3333333 -> 333.3333 cents -> 333
    const r3 = M.convert(M.toMinor('10', 'SGD'), 'SGD', 'USD', M.rateToScaled('0.33333333'));
    assert.equal(r3.converted, 333n);
    assert.equal(r3.rounded, true);
    assert.equal(M.toMajor(r3.converted, 'USD'), '3.33');

    // 负数保持符号
    assert.equal(M.convert(-81600000n, 'GBP', 'USD', rate).converted, -102000000n);
  });

  test('小于 1 的汇率合法，非法格式被拒', () => {
    assert.doesNotThrow(() => M.rateToScaled('0.79000000'));
    assert.throws(() => M.rateToScaled('0'));
    assert.throws(() => M.rateToScaled('-1.2'));
  });
});

describe('时区与截止', () => {
  test('挂钟时刻按条款时区换算（含夏令时）', () => {
    // 2026-03-31 17:00 伦敦处于 BST（UTC+1）-> 16:00Z
    assert.equal(T.zonedWallToUtc('2026-03-31T17:00', 'Europe/London'),
      Date.parse('2026-03-31T16:00:00Z'));
    // 新加坡 UTC+8 无夏令时：23:59 -> 15:59Z
    assert.equal(T.zonedWallToUtc('2026-05-15T23:59', 'Asia/Singapore'),
      Date.parse('2026-05-15T15:59:00Z'));
    // 纽约 2026-06-30 23:59 EDT（UTC-4）-> 2026-07-01 03:59Z
    assert.equal(T.zonedWallToUtc('2026-06-30T23:59', 'America/New_York'),
      Date.parse('2026-07-01T03:59:00Z'));
    // 冬季伦敦 GMT：17:00 -> 17:00Z
    assert.equal(T.zonedWallToUtc('2026-01-31T17:00', 'Europe/London'),
      Date.parse('2026-01-31T17:00:00Z'));
  });

  test('观察期期初按挂钟日历，不被 UTC 跨月干扰', () => {
    assert.equal(T.startOfPeriodWall('2026-06-30', 3), '2026-04-01');
    assert.equal(T.startOfPeriodWall('2026-03-31', 3), '2026-01-01');
    assert.equal(T.startOfPeriodWall('2026-12-31', 3), '2026-10-01');
  });

  test('宽限期判定', () => {
    const due = Date.parse('2026-05-15T16:00:00Z');
    assert.equal(T.deadlineStatus(Date.parse('2026-05-15T15:59:00Z'), due, 5), 'ontime');
    assert.equal(T.deadlineStatus(Date.parse('2026-05-16T00:00:00Z'), due, 5), 'within_grace');
    assert.equal(T.deadlineStatus(Date.parse('2026-05-21T00:00:00Z'), due, 5), 'late');
  });
});
