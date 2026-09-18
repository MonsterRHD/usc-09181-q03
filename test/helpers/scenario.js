'use strict';

// 验收用标准场景：海外子公司的银团贷款，三类负责人跨三个时区维护条款。
const path = require('path');
const os = require('os');
const fs = require('fs');
const { Station } = require('../../src/station');

function tmpDb(label) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'covenant-station-'));
  return path.join(dir, `${label}.json`);
}

function buildStation({ dbPath = tmpDb('scenario'), now = '2026-01-05T09:00:00Z' } = {}) {
  let t = Date.parse(now);
  const clock = () => t;
  const setNow = (iso) => { t = Date.parse(iso); };
  const station = Station.create(dbPath, { clock });

  const admin = { id: 'u-admin', name: '资金委员会管理员', role: 'facility_admin' };
  const legal = { id: 'u-legal', name: '法务负责人', role: 'legal' };
  const finance = { id: 'u-fin', name: '财务负责人', role: 'finance' };
  const business = { id: 'u-biz', name: '业务负责人', role: 'business' };
  const reviewer = { id: 'u-rev', name: '契约复核官', role: 'covenant_reviewer' };

  station.setupFacility({
    name: '海外子公司银团贷款',
    reportingCcy: 'USD',
    timeZone: 'UTC',
    reviewRole: 'covenant_reviewer',
    actor: admin,
  });

  for (const [p, tz, email] of [
    [legal, 'Europe/London', 'legal@sub.example'],
    [finance, 'America/New_York', 'finance@sub.example'],
    [business, 'Asia/Singapore', 'biz@sub.example'],
    [reviewer, 'UTC', 'review@committee.example'],
    [admin, 'UTC', 'admin@committee.example'],
  ]) {
    station.registerParty({ id: p.id, name: p.name, role: p.role, email, timeZone: tz, actor: admin });
  }

  // 指标口径
  station.defineMetric({ id: 'ebitda', name: '息税折旧摊销前利润', keys: ['EBITDA'], kind: 'amount', actor: finance });
  station.defineMetric({ id: 'debt_service', name: '当期还本付息', keys: ['DEBT_SERVICE'], kind: 'amount', actor: legal });
  station.defineMetric({ id: 'collateral', name: '合格担保物价值', keys: ['COLLATERAL'], kind: 'amount', actor: business });
  station.defineMetric({ id: 'secured_debt', name: '有担保负债', keys: ['SECURED_DEBT'], kind: 'amount', actor: business });
  station.defineMetric({ id: 'restricted_use', name: '受限用途资金投放', keys: ['RESTRICTED_USE'], kind: 'amount', actor: finance });

  return { station, setNow, dbPath, parties: { admin, legal, finance, business, reviewer } };
}

// 三类条款：偿债（法务）/ 担保（业务）/ 资金用途（财务）
function issueStandardCovenants(station, admin, { dueDays = 45 } = {}) {
  const c1 = station.issueCovenant({
    code: 'DSCR', category: 'debt_service', title: '偿债保障比率',
    ownerPartyId: 'u-legal',
    numerator: ['ebitda'], denominator: ['debt_service'],
    operator: 'ge', threshold: '1.20', warnBandPct: 5,
    periodMonths: 3,
    due: { daysAfterPeriodEnd: dueDays, timeOfDay: '17:00', timeZone: 'Europe/London' },
    graceDays: 5, cureDays: 10,
    effectiveFromWall: '2026-01-01T00:00',
  }, admin);

  const c2 = station.issueCovenant({
    code: 'SCR', category: 'security', title: '担保覆盖率',
    ownerPartyId: 'u-biz',
    numerator: ['collateral'], denominator: ['secured_debt'],
    operator: 'ge', threshold: '1.10', warnBandPct: 5,
    periodMonths: 3,
    due: { daysAfterPeriodEnd: dueDays, timeOfDay: '23:59', timeZone: 'Asia/Singapore' },
    graceDays: 5, cureDays: 0,
    effectiveFromWall: '2026-01-01T00:00',
  }, admin);

  const c3 = station.issueCovenant({
    code: 'UOP', category: 'use_of_proceeds', title: '受限用途投放上限',
    ownerPartyId: 'u-fin',
    numerator: ['restricted_use'], denominator: null,
    operator: 'le', threshold: '5000000', warnBandPct: 5,
    periodMonths: 3,
    due: { daysAfterPeriodEnd: dueDays, timeOfDay: '23:59', timeZone: 'America/New_York' },
    graceDays: 5, cureDays: 0,
    effectiveFromWall: '2026-01-01T00:00',
  }, admin);

  return { dscr: c1.covenantId, scr: c2.covenantId, uop: c3.covenantId };
}

module.exports = { tmpDb, buildStation, issueStandardCovenants };
