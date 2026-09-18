'use strict';

const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const { buildStation, issueStandardCovenants } = require('./helpers/scenario');

// 验收场景二：补发报表
// 上次季度报告因三地时区/口径不同被错过通知；本场景验证：
//  1) 截止按条款时区挂钟解释，巡检在"超过宽限"时补发迟到通知；
//  2) 迟到补发的报表登记为 backfill 并进入版本链，仍可生成每期状态；
//  3) 同一文件重复上传只记 duplicate，不产生新版本、不触发重算；
//  4) 修订报表以 revision 取代旧版，重算生成新评估版本，历史版本保留可溯。
describe('场景二：时区截止巡检、迟到补发、重复上传与修订版本链', () => {
  test('跨时区截止 -> 迟到通知 -> 补发 -> 重复 -> 修订，版本关系完整', () => {
    const { station, setNow, parties } = buildStation();
    const ids = issueStandardCovenants(station, parties.admin);
    // 本场景只跟踪 DSCR 与 SCR 两条线（不同截止时区）。
    const trackers = [
      station.ensureTracker(ids.dscr, '2026-03-31', parties.admin),
      station.ensureTracker(ids.scr, '2026-03-31', parties.admin),
    ];
    const byId = Object.fromEntries(trackers.map((t) => [t.covenantId, t]));

    // SCR 截止：2026-05-15 23:59 新加坡 = 15:59 UTC；DSCR：17:00 伦敦(夏令时) = 16:00 UTC。
    assert.equal(new Date(byId[ids.scr].dueUtc).toISOString(), '2026-05-15T15:59:00.000Z');
    assert.equal(new Date(byId[ids.dscr].dueUtc).toISOString(), '2026-05-15T16:00:00.000Z');

    // 16:30 UTC：SCR 已过截止但都在宽限内（宽限 5 天）——只有宽限提醒，没有迟到告警。
    let fired = station.tick('2026-05-15T16:30:00Z');
    assert.equal(fired.length, 2);
    assert.ok(fired.every((n) => n.type === 'report_within_grace'));
    assert.equal(station.s.alerts.filter((a) => a.type === 'report_late').length, 0);

    // 超过宽限（SGR 宽限止于 05-20 15:59Z）：两条都迟到，责任人各收一条迟到通知；重复 tick 不再发。
    fired = station.tick('2026-05-21T09:00:00Z');
    const lateAlerts = station.s.alerts.filter((a) => a.type === 'report_late');
    assert.equal(lateAlerts.length, 2);
    const lateToBiz = station.s.notifications.filter((n) => n.type === 'report_late' && n.toPartyId === 'u-biz');
    assert.equal(lateToBiz.length, 1);
    assert.equal(station.tick('2026-05-22T09:00:00Z').length, 0);

    // 迟到补发 SCR（新加坡子公司，新元）。
    station.ingestRate({ ccy: 'SGD', rate: '0.74000000', asOfWall: '2026-03-31T00:00', actor: parties.business });
    const bufBackfill = Buffer.from('COLLATERAL,SGD,2000000\nSECURED_DEBT,SGD,1300000\n');
    const backfill = station.ingestReport({
      docKey: 'bank-sg-q1', fileName: 'sg_sub_q1.csv', periodEndWall: '2026-03-31',
      buffer: bufBackfill,
      lines: [
        { key: 'COLLATERAL', ccy: 'SGD', amount: '2000000' },
        { key: 'SECURED_DEBT', ccy: 'SGD', amount: '1300000' },
      ],
      kind: 'backfill', receivedAtIso: '2026-05-22T08:00:00Z', actor: parties.business,
    });
    assert.equal(backfill.report.kind, 'backfill');
    assert.equal(backfill.duplicated, false);

    setNow('2026-05-22T10:00:00Z');
    const out1 = station.assessPeriod('2026-03-31', { only: [ids.scr], actor: parties.business });
    const a1 = out1.assessments[0];
    // 1,480,000 / 962,000 = 1.5384…，合规；来源报表标记为 backfill 且被记为迟到件。
    assert.equal(a1.status, 'compliant');
    assert.ok(Number(a1.computed.display) > 1.538 && Number(a1.computed.display) < 1.539);
    assert.deepEqual(a1.basis.sources.map((s) => s.kind), ['backfill']);
    assert.deepEqual(a1.basis.reportsLate, [backfill.report.id]);

    // 同一文件重复上传：duplicate，不进版本链；重算结果复用同一评估。
    const dup = station.ingestReport({
      docKey: 'bank-sg-q1', fileName: 'sg_sub_q1_copy.csv', periodEndWall: '2026-03-31',
      buffer: bufBackfill,
      lines: [
        { key: 'COLLATERAL', ccy: 'SGD', amount: '2000000' },
        { key: 'SECURED_DEBT', ccy: 'SGD', amount: '1300000' },
      ],
      kind: 'original', receivedAtIso: '2026-05-23T08:00:00Z', actor: parties.business,
    });
    assert.equal(dup.duplicated, true);
    assert.equal(dup.report.status, 'duplicate');
    assert.equal(dup.original.id, backfill.report.id);
    const outDup = station.assessPeriod('2026-03-31', { only: [ids.scr], actor: parties.business });
    assert.equal(outDup.assessments[0].id, a1.id);

    // 修订报表（银行更正担保物估值为 SGD 1,700,000 → $1,258,000，比率 1.3077…）。
    const rev = station.ingestReport({
      docKey: 'bank-sg-q1', fileName: 'sg_sub_q1_revised.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('COLLATERAL,SGD,1700000\nSECURED_DEBT,SGD,1300000\n'),
      lines: [
        { key: 'COLLATERAL', ccy: 'SGD', amount: '1700000' },
        { key: 'SECURED_DEBT', ccy: 'SGD', amount: '1300000' },
      ],
      kind: 'revision', supersedesReportId: backfill.report.id,
      receivedAtIso: '2026-05-25T08:00:00Z', actor: parties.business,
    });
    assert.equal(rev.report.supersedes, backfill.report.id);
    assert.equal(rev.report.rootId, backfill.report.id);
    assert.equal(backfill.report.status, 'superseded');

    const out2 = station.assessPeriod('2026-03-31', { only: [ids.scr], actor: parties.business });
    const a2 = out2.assessments[0];
    assert.notEqual(a2.id, a1.id);
    assert.equal(a2.versionNo, 2);
    assert.equal(a2.supersedes, a1.id);
    assert.equal(a1.supersededBy, a2.id);
    assert.ok(Number(a2.computed.display) > 1.307 && Number(a2.computed.display) < 1.308);
    assert.deepEqual(a2.basis.sources.map((s) => s.kind), ['revision']);

    // 报表版本链：补发 -> 修订；重复件不在链上。
    const chain = station.reportChain(backfill.report.id);
    assert.deepEqual(chain.map((r) => r.kind), ['backfill', 'revision']);
    // 历史评估未被覆盖。
    assert.equal(station.s.assessments.filter((a) => a.code === 'SCR' && a.periodEndWall === '2026-03-31').length, 2);
  });
});
