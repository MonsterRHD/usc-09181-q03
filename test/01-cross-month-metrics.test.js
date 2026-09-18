'use strict';

const { test, describe } = require('node:test');
const assert = require('node:assert/strict');
const { buildStation, issueStandardCovenants } = require('./helpers/scenario');
const { Station } = require('../src/station');

// 验收场景一：跨月指标
// 季度指标由两个月分别到达的银行报表合并（英镑子公司 + 美元总部），经期末汇率换算后生成每期状态；
// 接近阈值时无论评估多少次只提醒一次；审计解释能还原口径、汇率版本与每条数据来源。
describe('场景一：跨月指标累计、期末汇率口径、接近阈值只提醒一次', () => {
  test('跨月分次到达的多币种报表合并为季度状态，且审计可解释', () => {
    const { station, setNow, parties } = buildStation();
    const ids = issueStandardCovenants(station, parties.admin);
    station.openPeriod('2026-03-31', parties.admin);

    // 期末汇率口径：只认 3/31 之前（含）录入的版本；4 月的新汇率不得改写一季度计算。
    station.ingestRate({ ccy: 'GBP', rate: '1.25000000', asOfWall: '2026-03-31T00:00', actor: parties.finance });

    // 2 月先到的英国子公司报表（跨月）：£816,000
    const r1 = station.ingestReport({
      docKey: 'bank-uk-q1', fileName: 'uk_sub_q1_partial.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('EBITDA,GBP,816000\n'),
      lines: [{ key: 'EBITDA', ccy: 'GBP', amount: '816000' }],
      kind: 'original', receivedAtIso: '2026-02-05T10:00:00Z', actor: parties.legal,
    });
    // 4 月到的总部汇总：$300,000；还本付息：$1,050,000
    const r2 = station.ingestReport({
      docKey: 'hq-us-q1', fileName: 'hq_consolidated.csv', periodEndWall: '2026-03-31',
      buffer: Buffer.from('EBITDA,USD,300000\nDEBT_SERVICE,USD,1050000\n'),
      lines: [
        { key: 'EBITDA', ccy: 'USD', amount: '300000' },
        { key: 'DEBT_SERVICE', ccy: 'USD', amount: '1050000' },
      ],
      kind: 'original', receivedAtIso: '2026-04-02T15:00:00Z', actor: parties.legal,
    });

    setNow('2026-05-10T09:00:00Z');
    const out1 = station.assessPeriod('2026-03-31', { only: [ids.dscr], actor: parties.legal });
    const a1 = out1.assessments[0];

    // £816,000 × 1.25 = $1,020,000，加 $300,000 = $1,320,000；除以 $1,050,000 = 1.2571…
    assert.equal(a1.status, 'near_threshold'); // 阈值 1.20，5% 带宽边缘为 1.26
    assert.ok(Number(a1.computed.display) > 1.257 && Number(a1.computed.display) < 1.258);
    assert.deepEqual(Object.keys(a1.fxSnapshot), ['GBP']);

    // 同样输入再评估两次：不得重复提醒、不得新增评估版本。
    const out2 = station.assessPeriod('2026-03-31', { only: [ids.dscr], actor: parties.legal });
    const out3 = station.assessPeriod('2026-03-31', { only: [ids.dscr], actor: parties.legal });
    assert.equal(out2.assessments[0].id, a1.id);
    assert.equal(out3.assessments[0].id, a1.id);
    const nearAlerts = station.s.alerts.filter((x) => x.type === 'near_threshold');
    const nearNotifs = station.s.notifications.filter((x) => x.type === 'near_threshold');
    assert.equal(nearAlerts.length, 1);
    assert.equal(nearNotifs.length, 1);
    assert.equal(nearNotifs[0].toPartyId, 'u-legal'); // 责任人是法务负责人

    // 审计解释：来源包含两份跨月报表、汇率快照、条款版本。
    const ex = station.explain(a1.id);
    const srcIds = ex.assessment.basis.sources.map((s) => s.id).sort();
    assert.deepEqual(srcIds, [r1.report.id, r2.report.id].sort());
    assert.equal(ex.assessment.fxSnapshot.GBP.fxId, station.rateAt('GBP', Date.parse('2026-03-31T23:59:59Z')).id);
    assert.ok(ex.covenantVersion.threshold === '1.20');

    // 4 月录入新汇率，历史计算不被覆盖：重跑仍返回同一评估、同一指纹。
    station.ingestRate({ ccy: 'GBP', rate: '1.31000000', asOfWall: '2026-04-15T00:00', actor: parties.finance });
    const out4 = station.assessPeriod('2026-03-31', { only: [ids.dscr], actor: parties.legal });
    assert.equal(out4.assessments[0].id, a1.id);
  });
});
