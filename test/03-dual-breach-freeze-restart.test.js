'use strict';

const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const { buildStation, issueStandardCovenants } = require('./helpers/scenario');
const { Station } = require('../src/station');

// 验收场景三：两条约束同时触发 + 隔日重启 + 条款修订
// 同一批次中偿债与资金用途两条约束同时确认违约：
//  一个批次、一个复核任务、两类相关提款冻结（无关类别仍可提款）；
// 进程隔日重启后，未决复核与下一次截止仍可追踪，冻结仍然生效；
// 管理员修订阈值必须留生效时间与影响范围，历史期计算不被新口径覆盖。
describe('场景三：双约束同批触发、相关提款冻结、重启追踪与条款修订', () => {
  test('同批双违约 -> 冻结/复核 -> 重启仍生效 -> 修订不覆盖历史', () => {
    const { station, setNow, parties, dbPath } = buildStation();
    const ids = issueStandardCovenants(station, parties.admin);
    station.openPeriod('2026-03-31', parties.admin);
    station.openPeriod('2026-06-30', parties.admin);

    // 先合规：DSCR 1.28（仅提醒带宽外）。
    station.ingestReport({
      docKey: 'hq-q1-v1', fileName: 'hq_q1_v1.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('EBITDA,USD,1280000\nDEBT_SERVICE,USD,1000000\nRESTRICTED_USE,USD,4800000\n'),
      lines: [
        { key: 'EBITDA', ccy: 'USD', amount: '1280000' },
        { key: 'DEBT_SERVICE', ccy: 'USD', amount: '1000000' },
        { key: 'RESTRICTED_USE', ccy: 'USD', amount: '4800000' },
      ],
      receivedAtIso: '2026-05-01T09:00:00Z', actor: parties.legal,
    });
    setNow('2026-05-10T09:00:00Z');
    let out = station.assessPeriod('2026-03-31', { actor: parties.legal });
    assert.equal(out.assessments.find((a) => a.code === 'DSCR').status, 'compliant');
    assert.equal(out.review, null);

    // 修订报表：EBITDA 下调至 1,220,000（DSCR=1.22，落入 5% 带宽 -> 接近提醒）。
    station.ingestReport({
      docKey: 'hq-q1-v1', fileName: 'hq_q1_v2.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('EBITDA,USD,1220000\nDEBT_SERVICE,USD,1000000\nRESTRICTED_USE,USD,4800000\n'),
      lines: [
        { key: 'EBITDA', ccy: 'USD', amount: '1220000' },
        { key: 'DEBT_SERVICE', ccy: 'USD', amount: '1000000' },
        { key: 'RESTRICTED_USE', ccy: 'USD', amount: '4800000' },
      ],
      kind: 'revision',
      supersedesReportId: station.s.reports.find((r) => r.fileName === 'hq_q1_v1.csv').id,
      receivedAtIso: '2026-05-11T09:00:00Z', actor: parties.legal,
    });
    out = station.assessPeriod('2026-03-31', { actor: parties.legal });
    assert.equal(out.assessments.find((a) => a.code === 'DSCR').status, 'near_threshold');
    assert.equal(station.s.alerts.filter((a) => a.type === 'near_threshold' && a.covenantId === ids.dscr && a.status === 'active').length, 1);

    // 再次修订：EBITDA 1,100,000（DSCR=1.10 违约）且受限用途 5,200,000 超上限——两条同批触发。
    station.ingestReport({
      docKey: 'hq-q1-v1', fileName: 'hq_q1_v3.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('EBITDA,USD,1100000\nDEBT_SERVICE,USD,1000000\nRESTRICTED_USE,USD,5200000\n'),
      lines: [
        { key: 'EBITDA', ccy: 'USD', amount: '1100000' },
        { key: 'DEBT_SERVICE', ccy: 'USD', amount: '1000000' },
        { key: 'RESTRICTED_USE', ccy: 'USD', amount: '5200000' },
      ],
      kind: 'revision',
      supersedesReportId: station.s.reports.find((r) => r.fileName === 'hq_q1_v2.csv').id,
      receivedAtIso: '2026-05-12T09:00:00Z', actor: parties.legal,
    });
    out = station.assessPeriod('2026-03-31', { actor: parties.legal });
    const breached = out.assessments.filter((a) => a.status === 'breach');
    assert.deepEqual(breached.map((a) => a.code).sort(), ['DSCR', 'UOP']);
    // 同一批次号。
    assert.equal(new Set(breached.map((a) => a.batchId)).size, 1);
    // 早先的接近提醒已被撤回，违约告警两条且均只通知一次。
    assert.equal(station.s.alerts.find((a) => a.covenantId === ids.dscr && a.type === 'near_threshold').status, 'withdrawn');
    assert.equal(station.s.alerts.filter((a) => a.type === 'breach' && a.status === 'active').length, 2);
    station.assessPeriod('2026-03-31', { actor: parties.legal });
    assert.equal(station.s.alerts.filter((a) => a.type === 'breach' && a.status === 'active').length, 2);

    // 一个复核任务，覆盖两条约束；两个冻结分别挂偿债与资金用途类别。
    assert.ok(out.review);
    assert.equal(out.review.status, 'open');
    assert.equal(out.review.requiredRole, 'covenant_reviewer');
    assert.deepEqual(out.review.covenantIds.sort(), [ids.dscr, ids.uop].sort());
    const freezes = station.s.freezes.filter((f) => f.status === 'active');
    assert.deepEqual(freezes.map((f) => f.category).sort(), ['debt_service', 'use_of_proceeds']);

    // 提款：相关类别被冻结拦截；无关类别照常批准；拦截记录引用具体冻结。
    const blocked = station.requestDrawdown({ amount: '500000', ccy: 'USD', category: 'use_of_proceeds', purpose: '新增受限投放', actor: parties.finance });
    assert.equal(blocked.status, 'blocked');
    assert.ok(blocked.blockedBy.includes(freezes.find((f) => f.category === 'use_of_proceeds').id));
    const blocked2 = station.requestDrawdown({ amount: '100000', ccy: 'USD', category: 'debt_service', actor: parties.legal });
    assert.equal(blocked2.status, 'blocked');
    const ok = station.requestDrawdown({ amount: '200000', ccy: 'USD', category: 'working_capital', purpose: '日常周转', actor: parties.business });
    assert.equal(ok.status, 'approved');

    // 法务无权复核。
    assert.throws(
      () => station.resolveReview(out.review.id, { outcome: 'confirmed', actor: parties.legal }),
      (e) => e.code === 'ROLE_REQUIRED',
    );

    // ---- 隔日重启：新进程从磁盘恢复 ----
    setNow('2026-05-13T08:00:00Z');
    const next = Station.open(dbPath, { clock: () => Date.parse('2026-05-13T08:00:00Z') });
    const pending = next.pendingReviews();
    assert.equal(pending.openReviews.length, 1);
    assert.equal(pending.openReviews[0].id, out.review.id);
    assert.deepEqual(pending.openReviews[0].covenantIds.sort(), [ids.dscr, ids.uop].sort());
    // 下一次截止：Q2（2026-06-30 期）三个跟踪器在列，且 Q1 未决复核仍可追踪。
    const q2 = pending.deadlines.filter((d) => d.periodEndWall === '2026-06-30');
    assert.equal(q2.length, 3);
    // 重启后冻结依旧拦截。
    const blockedAfterRestart = next.requestDrawdown({ amount: '1', ccy: 'USD', category: 'debt_service', actor: parties.legal });
    assert.equal(blockedAfterRestart.status, 'blocked');

    // ---- 管理员修订 UOP 阈值：6,000,000，自 Q2 起生效，不追溯当期 ----
    const amended = next.amendCovenant(ids.uop, {
      threshold: '6000000',
      effectiveFromWall: '2026-04-01T00:00',
      impactScope: 'future_periods',
    }, parties.admin);
    assert.equal(amended.threshold, '6000000');
    assert.equal(amended.impactScope, 'future_periods');
    // 非管理员不能修订。
    assert.throws(() => next.amendCovenant(ids.uop, { threshold: '999', effectiveFromWall: '2026-04-01T00:00' }, parties.finance),
      (e) => e.code === 'FORBIDDEN');

    // Q1 历史评估仍是 520 万对旧阈值 500 万的违约，不被新口径覆盖。
    const q1History = next.s.assessments.filter((a) => a.code === 'UOP' && a.periodEndWall === '2026-03-31');
    const q1Latest = q1History[q1History.length - 1];
    assert.equal(q1Latest.threshold, '5000000');
    assert.equal(q1Latest.status, 'breach');
    const vQ1 = next.versionFor(ids.uop, Date.parse('2026-03-31T23:59:59Z'), Date.parse('2026-05-13T08:00:00Z'));
    assert.equal(vQ1.threshold, '5000000');
    // Q2 期初按条款时区挂钟为 2026-04-01（纽约时区即 04:00Z），与生效时刻相等 -> 适用新版本。
    const q2TrackerUop = next.s.trackers.find((t) => t.covenantId === ids.uop && t.periodEndWall === '2026-06-30');
    const vQ2 = next.versionFor(ids.uop, q2TrackerUop.periodEndUtc, Date.parse('2026-05-13T08:00:00Z'), q2TrackerUop.periodStartUtc);
    assert.equal(vQ2.threshold, '6000000');

    // 审计链：修订记录含生效时间、影响范围、变更字段。
    const amendLog = next.store.auditTrail({ entity: 'covenant', entityId: ids.uop })
      .find((e) => e.action === 'covenant.amend');
    assert.equal(amendLog.detail.impactScope, 'future_periods');
    assert.deepEqual(amendLog.detail.changedFields, ['threshold']);
    assert.equal(amendLog.actor.role, 'facility_admin');

    // 复核官确认违约：维持冻结，复核关闭。
    const confirmed = next.resolveReview(out.review.id, { outcome: 'confirmed', note: '银行修订数确认', actor: parties.reviewer });
    assert.equal(confirmed.status, 'confirmed');
    assert.equal(next.s.freezes.filter((f) => f.status === 'active').length, 2);
  });
});
