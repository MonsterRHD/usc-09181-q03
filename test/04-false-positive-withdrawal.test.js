'use strict';

const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const { buildStation, issueStandardCovenants } = require('./helpers/scenario');

// 验收场景四：撤回误报
// 银行报表把非受限资金投放误记为受限用途，UOP 被判定违约并冻结；
// 复核官认定为误报后：违约告警撤回、冻结解除、提款恢复；
// 评估以"撤回-误报"新版本留痕（原违约数字不删改）；同输入重跑不得让误报复活；
// 银行随后补发正确报表，系统重新走正常的合规评估链路，全程审计可解释。
describe('场景四：确认违约后的误报撤回与审计解释', () => {
  test('误报撤回：通知撤回、解冻、终态不复活，更正后重新合规', () => {
    const { station, setNow, parties } = buildStation();
    const ids = issueStandardCovenants(station, parties.admin);
    station.openPeriod('2026-03-31', parties.admin);

    // 误报来源：银行把一笔 5,200,000 的普通调拨记成受限用途。
    const wrong = station.ingestReport({
      docKey: 'bank-uop-q1', fileName: 'bank_uop_q1_wrong.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('RESTRICTED_USE,USD,5200000\n'),
      lines: [{ key: 'RESTRICTED_USE', ccy: 'USD', amount: '5200000' }],
      receivedAtIso: '2026-05-10T09:00:00Z', actor: parties.finance,
    });
    setNow('2026-05-10T10:00:00Z');
    const out1 = station.assessPeriod('2026-03-31', { only: [ids.uop], actor: parties.finance });
    const breachAsm = out1.assessments[0];
    assert.equal(breachAsm.status, 'breach');
    const review = out1.review;
    assert.ok(review && review.status === 'open');
    const freezeId = station.s.freezes.find((f) => f.status === 'active').id;

    // 冻结中提款被拦截。
    const blocked = station.requestDrawdown({ amount: '300000', ccy: 'USD', category: 'use_of_proceeds', actor: parties.finance });
    assert.equal(blocked.status, 'blocked');

    // 指定角色（复核官）认定误报：撤回告警、解冻。
    const resolved = station.resolveReview(review.id, {
      outcome: 'false_positive',
      note: '经核实该笔为集团内部普通调拨，银行科目误记，非受限用途',
      actor: parties.reviewer,
    });
    assert.equal(resolved.status, 'false_positive');

    const breachAlert = station.s.alerts.find((a) => a.type === 'breach' && a.covenantId === ids.uop);
    assert.equal(breachAlert.status, 'withdrawn');
    assert.equal(breachAlert.withdrawnBy, 'u-rev');
    assert.ok(breachAlert.withdrawReason.includes('普通调拨'));
    const freeze = station.s.freezes.find((f) => f.id === freezeId);
    assert.equal(freeze.status, 'lifted');
    assert.equal(freeze.liftReason, 'false_positive');

    // 提款恢复。
    const approved = station.requestDrawdown({ amount: '300000', ccy: 'USD', category: 'use_of_proceeds', actor: parties.finance });
    assert.equal(approved.status, 'approved');

    // 评估新版本链：v1 breach 原样保留，v2 withdrawn_false_positive 取代之。
    const versions = station.s.assessments.filter((a) => a.code === 'UOP' && a.periodEndWall === '2026-03-31')
      .sort((x, y) => x.versionNo - y.versionNo);
    assert.equal(versions.length, 2);
    assert.equal(versions[0].status, 'breach');
    assert.equal(versions[1].status, 'withdrawn_false_positive');
    assert.equal(versions[1].supersedes, versions[0].id);
    assert.equal(versions[0].supersededBy, versions[1].id);
    assert.equal(versions[1].withdrawnBy, 'u-rev');

    // 同一错误输入重跑：误报不得复活（人工裁断是终态，直到数据被更正）。
    const out2 = station.assessPeriod('2026-03-31', { only: [ids.uop], actor: parties.finance });
    assert.equal(out2.assessments[0].id, versions[1].id);
    assert.equal(station.s.alerts.filter((a) => a.type === 'breach' && a.status === 'active').length, 0);
    assert.equal(station.s.freezes.filter((f) => f.status === 'active').length, 0);
    assert.equal(station.s.reviews.filter((r) => r.status === 'open').length, 0);

    // 银行补发正确报表（修订）：数字更正为 4,200,000，重新走正常评估，结论合规。
    station.ingestReport({
      docKey: 'bank-uop-q1', fileName: 'bank_uop_q1_correct.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('RESTRICTED_USE,USD,4200000\n'),
      lines: [{ key: 'RESTRICTED_USE', ccy: 'USD', amount: '4200000' }],
      kind: 'revision', supersedesReportId: wrong.report.id,
      receivedAtIso: '2026-05-14T09:00:00Z', actor: parties.finance,
    });
    setNow('2026-05-14T10:00:00Z');
    const out3 = station.assessPeriod('2026-03-31', { only: [ids.uop], actor: parties.finance });
    const a3 = out3.assessments[0];
    assert.equal(a3.status, 'compliant');
    assert.equal(a3.versionNo, 3);
    assert.equal(a3.supersedes, versions[1].id);
    assert.equal(a3.computed.display, '4200000.00 USD');
    // 合规后不产生新违约/新复核。
    assert.equal(station.s.alerts.filter((a) => a.type === 'breach' && a.status === 'active').length, 0);
    assert.equal(station.s.reviews.filter((r) => r.status === 'open').length, 0);

    // 审计解释链完整：违约 -> 复核撤回 -> 修订 -> 合规，每一步都有人、时点、依据。
    const actions = station.store.auditTrail({ entity: 'review', entityId: review.id }).map((e) => e.action);
    assert.ok(actions.includes('review.resolve'));
    const resolveLog = station.store.auditTrail({ entity: 'review', entityId: review.id })
      .find((e) => e.action === 'review.resolve');
    assert.equal(resolveLog.detail.outcome, 'false_positive');
    const ex = station.explain(a3.id);
    assert.equal(ex.assessment.basis.sources[0].fileName, 'bank_uop_q1_correct.csv');
    // 原违约评估仍可被审计解释（数字与依据都未被抹掉）。
    const exOld = station.explain(versions[0].id);
    assert.equal(exOld.assessment.computed.display, '5200000.00 USD');
    assert.equal(exOld.assessment.basis.sources[0].fileName, 'bank_uop_q1_wrong.csv');
  });
});
