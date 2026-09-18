'use strict';

const { Store } = require('./store');
const T = require('./time');
const M = require('./money');

// 融资契约观察站
// 把银团合同拆成：授信框架 + 角色 + 条款（版本化的可计算约束）+ 指标口径 + 观察周期/截止/宽限。
// 输入：银行报表（版本链）与人工佐证；输出：每期状态、一次性提醒、违约冻结与复核任务，全程留痕。

class StationError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

const ROLES = {
  LEGAL: 'legal',                 // 法务：偿债类条款
  FINANCE: 'finance',             // 财务：资金用途 + 汇率口径
  BUSINESS: 'business',           // 业务负责人：担保类条款
  ADMIN: 'facility_admin',        // 资金委员会管理员：条款修订
  REVIEWER: 'covenant_reviewer',  // 指定复核角色
};

let _warned = new Set();

class Station {
  constructor(store) {
    this.store = store;
    this.s = store.state;
    this.clock = store.clock;
  }

  static create(filePath, opts) {
    return new Station(new Store(filePath, opts));
  }

  // 模拟隔日重启：从磁盘原子状态文件恢复全部跟踪、未决复核与审计链。
  static open(filePath, opts) {
    return new Station(Store.reopen(filePath, opts));
  }

  // ---------- 主体与角色 ----------

  setupFacility({ name, reportingCcy = 'USD', timeZone = 'UTC', reviewRole = ROLES.REVIEWER, actor }) {
    if (this.s.facility) throw new StationError('FACILITY_EXISTS', '授信框架已初始化');
    this.s.facility = {
      name, reportingCcy, timeZone, reviewRole,
      createdAt: this.clock(),
    };
    this.store.audit({ actor, action: 'facility.setup', entity: 'facility', detail: { name, reportingCcy, timeZone, reviewRole } });
    this.store.save();
    return this.s.facility;
  }

  registerParty({ id, name, role, email = null, timeZone = null, actor }) {
    if (this.s.parties[id]) throw new StationError('PARTY_EXISTS', `参与方已存在: ${id}`);
    const party = { id, name, role, email, timeZone, active: true, registeredAt: this.clock() };
    this.s.parties[id] = party;
    this.store.audit({ actor: actor || party, action: 'party.register', entity: 'party', entityId: id, detail: { name, role } });
    this.store.save();
    return party;
  }

  // ---------- 口径：指标定义与汇率 ----------

  defineMetric({ id, name, keys, kind = 'amount', actor }) {
    if (this.s.metricDefs.some((m) => m.id === id)) throw new StationError('METRIC_EXISTS', id);
    const metric = { id, name, keys: [...keys], kind, createdAt: this.clock() };
    this.s.metricDefs.push(metric);
    this.store.audit({ actor, action: 'metric.define', entity: 'metric', entityId: id, detail: { name, keys } });
    this.store.save();
    return metric;
  }

  // 汇率不可变追加；评估时只取"锚点时点之前"的最后一条，新汇率不会改写历史计算。
  ingestRate({ ccy, rate, asOfWall, timeZone, source = 'manual', actor }) {
    if (ccy === this.s.facility.reportingCcy) throw new StationError('RATE_REDUNDANT', '报告币种无需汇率');
    const scaled = M.rateToScaled(rate);
    const effectiveAt = T.zonedWallToUtc(asOfWall, timeZone || this.s.facility.timeZone);
    const id = this.store.nextId('fx');
    const rec = { id, ccy, rate, rateScaled: scaled, asOfWall, timeZone: timeZone || this.s.facility.timeZone, effectiveAt, source, ingestedAt: this.clock() };
    this.s.fxRates.push(rec);
    this.store.audit({ actor, action: 'fx.ingest', entity: 'fxRate', entityId: id, detail: { ccy, rate, asOfWall } });
    this.store.save();
    return rec;
  }

  rateAt(ccy, anchorUtc) {
    if (ccy === this.s.facility.reportingCcy) return { rateScaled: M.RATE_SCALE, native: true };
    const candidates = this.s.fxRates
      .filter((r) => r.ccy === ccy && r.effectiveAt <= anchorUtc)
      .sort((a, b) => a.effectiveAt - b.effectiveAt || b.ingestedAt - a.ingestedAt);
    if (candidates.length === 0) return null;
    return candidates[candidates.length - 1];
  }

  // ---------- 条款（带版本链，修订留生效时间与影响范围） ----------

  /**
   * spec:
   *  code/category/title/ownerPartyId
   *  numerator: [metricId], denominator: [metricId]|null
   *  operator 'ge'|'le', threshold 字符串（比率如 '1.20'，金额为报告币种 major 数）
   *  warnBandPct 接近阈值的提醒带宽（百分比）
   *  periodMonths 观察周期；due: {daysAfterPeriodEnd, timeOfDay, timeZone}；graceDays 报表宽限；cureDays 违约补救期
   *  fxPolicy 'period_end'（默认，锚定期末汇率）
   */
  issueCovenant(spec, actor) {
    this._assertActor(actor, [ROLES.ADMIN]);
    return this._addCovenantVersion(spec, { effectiveFromWall: spec.effectiveFromWall, supersedes: null, impactScope: null }, actor);
  }

  // 修订：新增版本行，旧版本标记 superseded；历史期是否沿用旧版由 impactScope 决定。
  amendCovenant(covenantId, patch, actor) {
    this._assertActor(actor, [ROLES.ADMIN]);
    const current = this.currentVersion(covenantId);
    if (!current) throw new StationError('COVENANT_NOT_FOUND', covenantId);
    if (!patch.effectiveFromWall) throw new StationError('EFFECTIVE_REQUIRED', '修订必须给出生效时间');
    const scope = patch.impactScope === 'current_and_future' ? 'current_and_future' : 'future_periods';
    const merged = { ...current, ...patch, code: current.code, category: current.category };
    const v = this._addCovenantVersion(merged, { effectiveFromWall: patch.effectiveFromWall, supersedes: current.versionId, impactScope: scope }, actor);
    current.status = 'superseded';
    this.store.audit({
      actor, action: 'covenant.amend', entity: 'covenant', entityId: covenantId,
      detail: { newVersion: v.versionId, supersedes: current.versionId, effectiveFromWall: patch.effectiveFromWall, impactScope: scope, changedFields: Object.keys(patch).filter((k) => !['effectiveFromWall', 'impactScope'].includes(k)) },
    });
    this.store.save();
    return v;
  }

  _addCovenantVersion(spec, ver, actor) {
    if (!this.s.parties[spec.ownerPartyId]) throw new StationError('OWNER_NOT_FOUND', spec.ownerPartyId);
    const due = spec.due || {};
    const versionId = this.store.nextId('cv');
    const rec = {
      covenantId: spec.covenantId || this.store.nextId('cov'),
      versionId,
      code: spec.code,
      category: spec.category, // debt_service | security | use_of_proceeds
      title: spec.title,
      ownerPartyId: spec.ownerPartyId,
      numerator: [...spec.numerator],
      denominator: spec.denominator ? [...spec.denominator] : null,
      operator: spec.operator || 'ge',
      threshold: spec.threshold,
      warnBandPct: spec.warnBandPct ?? 5,
      periodMonths: spec.periodMonths || 3,
      due: { daysAfterPeriodEnd: due.daysAfterPeriodEnd ?? 45, timeOfDay: due.timeOfDay || '23:59', timeZone: due.timeZone || this.s.facility.timeZone },
      graceDays: spec.graceDays ?? 0,
      cureDays: spec.cureDays ?? 0,
      fxPolicy: spec.fxPolicy || 'period_end',
      effectiveFromUtc: T.zonedWallToUtc(ver.effectiveFromWall || '1970-01-01T00:00', (spec.due && spec.due.timeZone) || this.s.facility.timeZone),
      impactScope: ver.impactScope,
      supersedes: ver.supersedes,
      status: 'active',
      issuedAt: this.clock(),
    };
    this.s.covenants.push(rec);
    if (!ver.supersedes) {
      this.store.audit({ actor, action: 'covenant.issue', entity: 'covenant', entityId: rec.covenantId, detail: { versionId, code: rec.code } });
      this.store.save();
    }
    return rec;
  }

  covenantVersions(covenantId) {
    return this.s.covenants.filter((c) => c.covenantId === covenantId).sort((a, b) => a.effectiveFromUtc - b.effectiveFromUtc);
  }

  currentVersion(covenantId) {
    const actives = this.s.covenants.filter((c) => c.covenantId === covenantId && c.status === 'active');
    return actives[actives.length - 1] || null;
  }

  // 为某一期选择适用版本：
  //  - future_periods：仅当该期"期初（按条款时区挂钟）"不早于生效时刻才适用新版本
  //  - current_and_future：该期"期末"不早于生效时刻即可
  //  - 无声明（首次签发）：生效即可用
  versionFor(covenantId, periodEndUtc, atUtc, periodStartUtc = null) {
    const vs = this.covenantVersions(covenantId).filter((v) => v.effectiveFromUtc <= atUtc);
    let chosen = vs[0];
    for (const v of vs) {
      if (!v.impactScope) { chosen = v; continue; }
      const pStart = periodStartUtc !== null
        ? periodStartUtc
        : T.startOfPeriodUtc(periodEndUtc, v.periodMonths);
      if (v.impactScope === 'current_and_future') {
        if (periodEndUtc >= v.effectiveFromUtc) chosen = v;
      } else if (pStart >= v.effectiveFromUtc) {
        chosen = v;
      }
    }
    return chosen;
  }

  // ---------- 观察期与跟踪 ----------

  ensureTracker(covenantId, periodEndWall, actor = null) {
    const version = this.currentVersion(covenantId);
    if (!version) throw new StationError('COVENANT_NOT_FOUND', covenantId);
    const periodEndUtc = T.zonedWallToUtc(`${periodEndWall}T23:59:59`, version.due.timeZone);
    const existing = this.s.trackers.find((t) => t.covenantId === covenantId && t.periodEndUtc === periodEndUtc);
    if (existing) return existing;
    const dueWall = this._dueWallDate(periodEndWall, version.due.daysAfterPeriodEnd);
    const dueUtc = T.zonedWallToUtc(`${dueWall}T${version.due.timeOfDay}`, version.due.timeZone);
    const periodStartWall = T.startOfPeriodWall(periodEndWall, version.periodMonths);
    const tracker = {
      id: this.store.nextId('trk'),
      covenantId,
      periodEndWall,
      periodStartWall,
      periodStartUtc: T.zonedWallToUtc(`${periodStartWall}T00:00`, version.due.timeZone),
      periodEndUtc,
      dueUtc,
      graceEndUtc: dueUtc + version.graceDays * T.DAY_MS,
      cureEndUtc: null,
      state: 'awaiting_report',
      latestAssessmentId: null,
      reportLateNotice: null,
      graceNotice: null,
      createdAt: this.clock(),
    };
    this.s.trackers.push(tracker);
    this.store.audit({ actor, action: 'tracker.open', entity: 'tracker', entityId: tracker.id, detail: { covenantId, periodEndWall, dueWall } });
    this.store.save();
    return tracker;
  }

  _dueWallDate(periodEndWall, days) {
    const d = new Date(T.zonedWallToUtc(`${periodEndWall}T00:00`, 'UTC'));
    d.setUTCDate(d.getUTCDate() + days);
    return d.toISOString().slice(0, 10);
  }

  // 开立某观察期的全部条款跟踪器（季度初排程用，不必等报表到达）。
  openPeriod(periodEndWall, actor = null) {
    return [...new Set(this.s.covenants.map((c) => c.covenantId))]
      .map((id) => this.ensureTracker(id, periodEndWall, actor));
  }

  // 时钟巡检：跨时区截止判定 + 迟到/宽限通知（修复"季度报告错过通知"）。
  tick(nowIso = null, actor = null) {
    const now = nowIso ? T.utc(nowIso) : this.clock();
    const fired = [];
    for (const t of this.s.trackers) {
      if (t.latestAssessmentId || t.reportLateNotice) continue;
      const owner = this._ownerOf(t.covenantId);
      if (!t.graceNotice && now > t.dueUtc && now <= t.graceEndUtc) {
        const n = this._notify('report_within_grace', t, owner, now,
          `报表进入宽限期：${this._cvLabel(t.covenantId)} 期 ${t.periodEndWall}`, { dueUtc: t.dueUtc, graceEndUtc: t.graceEndUtc });
        t.graceNotice = n.id;
        t.state = 'within_grace';
        fired.push(n);
      } else if (now > t.graceEndUtc) {
        const a = this._alert({ type: 'report_late', covenantId: t.covenantId, periodEndUtc: t.periodEndUtc, severity: 'warning', message: `报表超过宽限期仍未收到：${this._cvLabel(t.covenantId)}` }, now);
        const n = this._notifyAlert(a, owner, now);
        t.reportLateNotice = n.id;
        t.state = 'report_late';
        fired.push(n);
      }
    }
    if (fired.length) {
      this.store.audit({ actor, action: 'tick.deadline', entity: 'tick', detail: { fired: fired.length } });
      this.store.save();
    }
    return fired;
  }

  // ---------- 银行报表（版本链：迟到/补发/修订/同文件重复） ----------

  /**
   * report: {docKey, fileName, periodEndWall, buffer|sha256, lines:[{key,ccy,amount}],
   *          kind:'original'|'backfill'|'revision', receivedAtIso, supersedesReportId?}
   * 同一文件（sha256 相同）重复上传：只登记 duplicate，不生成新版本、不触发重算。
   */
  ingestReport({ docKey, fileName, buffer = null, sha256 = null, periodEndWall, lines, kind = 'original', supersedesReportId = null, receivedAtIso = null, actor }) {
    if (!actor) throw new StationError('ACTOR_REQUIRED', 'ingestReport 需要 actor');
    const receivedAt = receivedAtIso ? T.utc(receivedAtIso) : this.clock();
    const hash = sha256 || this.store.hashBuffer(buffer);

    const dup = this.s.reports.find((r) => r.periodEndWall === periodEndWall && r.sha256 === hash && r.status !== 'duplicate');
    if (dup) {
      const rec = {
        id: this.store.nextId('rep'), docKey, fileName, periodEndWall, sha256: hash,
        kind: 'duplicate', duplicateOf: dup.id, status: 'duplicate',
        lines: [], receivedAt, uploadedBy: actor.id,
      };
      this.s.reports.push(rec);
      this.store.audit({ actor, action: 'report.duplicate', entity: 'report', entityId: rec.id, detail: { duplicateOf: dup.id, fileName } });
      this.store.save();
      return { duplicated: true, report: rec, original: dup };
    }

    let rootId = null;
    if (supersedesReportId) {
      const prev = this.s.reports.find((r) => r.id === supersedesReportId);
      if (!prev) throw new StationError('PREV_NOT_FOUND', supersedesReportId);
      if (prev.periodEndWall !== periodEndWall) throw new StationError('PERIOD_MISMATCH', '修订报表的观察期必须一致');
      rootId = prev.rootId || prev.id;
      prev.status = 'superseded';
      prev.supersededByPending = true;
    }

    const rec = {
      id: this.store.nextId('rep'),
      docKey: docKey || `doc-${periodEndWall}`,
      rootId,
      fileName, periodEndWall, sha256: hash,
      kind, // original | backfill | revision | duplicate
      supersedes: supersedesReportId,
      status: 'accepted',
      lines: lines.map((l) => ({ key: l.key, ccy: l.ccy, amountMinor: M.toMinor(l.amount, l.ccy) })),
      receivedAt,
      uploadedBy: actor.id,
    };
    if (!rootId) rec.rootId = rec.id;
    this.s.reports.push(rec);
    this.store.audit({
      actor, action: 'report.ingest', entity: 'report', entityId: rec.id,
      detail: { docKey: rec.docKey, fileName, periodEndWall, kind, supersedes: supersedesReportId, late: this._lateFlag(periodEndWall, receivedAt) },
    });
    this.store.save();
    return { duplicated: false, report: rec };
  }

  reportChain(docRootId) {
    return this.s.reports
      .filter((r) => (r.rootId || r.id) === docRootId && r.kind !== 'duplicate')
      .sort((a, b) => a.receivedAt - b.receivedAt);
  }

  _lateFlag(periodEndWall, receivedAt) {
    const flags = [];
    for (const cv of this.s.covenants.filter((c) => c.status === 'active')) {
      const dueWall = this._dueWallDate(periodEndWall, cv.due.daysAfterPeriodEnd);
      const dueUtc = T.zonedWallToUtc(`${dueWall}T${cv.due.timeOfDay}`, cv.due.timeZone);
      if (receivedAt > dueUtc + cv.graceDays * T.DAY_MS) flags.push({ covenantId: cv.covenantId, dueUtc, late: true });
    }
    return flags;
  }

  // ---------- 人工佐证（可更正/撤回） ----------

  ingestEvidence({ periodEndWall, key, ccy, amount, note = '', supersedesEvidenceId = null, status: st = 'active', receivedAtIso = null, actor }) {
    if (!actor) throw new StationError('ACTOR_REQUIRED', 'ingestEvidence 需要 actor');
    const receivedAt = receivedAtIso ? T.utc(receivedAtIso) : this.clock();
    if (supersedesEvidenceId) {
      const prev = this.s.evidences.find((e) => e.id === supersedesEvidenceId);
      if (!prev) throw new StationError('PREV_NOT_FOUND', supersedesEvidenceId);
      if (prev.key !== key) throw new StationError('KEY_MISMATCH', '佐证更正必须针对同一指标项');
      prev.status = 'corrected';
    }
    const rec = {
      id: this.store.nextId('ev'),
      periodEndWall, key, ccy,
      amountMinor: M.toMinor(amount, ccy),
      note, supersedes: supersedesEvidenceId,
      status: st, // active | corrected | withdrawn
      providedBy: actor.id, receivedAt,
    };
    this.s.evidences.push(rec);
    this.store.audit({ actor, action: 'evidence.ingest', entity: 'evidence', entityId: rec.id, detail: { periodEndWall, key, ccy, amount, supersedes: supersedesEvidenceId } });
    this.store.save();
    return rec;
  }

  // ---------- 评估 ----------

  /**
   * 对一个观察期做整批评估（同一批次可同时触发多条约束）。
   * atIso：评估时点（决定适用条款版本）；历史评估永不被覆盖，结果以版本链追加。
   */
  assessPeriod(periodEndWall, { atIso = null, actor, only = null, force = false } = {}) {
    const at = atIso ? T.utc(atIso) : this.clock();
    const covenantIds = only
      ? [...new Set(only)]
      : [...new Set(this.s.covenants.map((c) => c.covenantId))];
    const trackers = covenantIds.map((id) => this.ensureTracker(id, periodEndWall, actor));

    const sources = this._collectSources(periodEndWall);
    const batchId = this.store.nextId('batch');
    const assessments = [];

    for (const covenantId of covenantIds) {
      const tracker = trackers.find((t) => t.covenantId === covenantId);
      const version = this.versionFor(covenantId, tracker.periodEndUtc, at, tracker.periodStartUtc ?? null);
      const a = this._assessOne({ batchId, covenantId, version, tracker, periodEndWall, sources, at, actor, force });
      assessments.push(a);
    }

    // 已经人工"确认违约"的条款，同输入重评不再重复开单/重复告警；冻结本来就持续。
    const confirmedIds = new Set(this.s.reviews
      .filter((r) => r.status === 'confirmed' && r.periodEndWall === periodEndWall)
      .flatMap((r) => r.covenantIds));
    const breaches = assessments.filter((a) => a.status === 'breach' && !confirmedIds.has(a.covenantId));
    let review = this.s.reviews.find((r) => r.status === 'open' && r.periodEndWall === periodEndWall) || null;

    if (review) {
      // 修订重评后收口已有复核：剔除已恢复合规的条款；全部恢复则以 revision_cleared 关闭。
      const breachIds = new Set(breaches.map((b) => b.covenantId));
      const removed = review.covenantIds.filter((id) => !breachIds.has(id));
      if (removed.length) {
        review.covenantIds = review.covenantIds.filter((id) => breachIds.has(id));
        const removedAsm = new Set(this.s.assessments.filter((a) => removed.includes(a.covenantId) && a.periodEndWall === periodEndWall).map((a) => a.id));
        review.assessmentIds = review.assessmentIds.filter((id) => !removedAsm.has(id));
        review.freezeIds = review.freezeIds.filter((fid) => {
          const f = this.s.freezes.find((x) => x.id === fid);
          return f && f.status === 'active';
        });
        for (const t of this.s.trackers.filter((x) => x.periodEndWall === periodEndWall && removed.includes(x.covenantId))) {
          t.reviewId = null;
        }
        this.store.audit({ actor, action: 'review.covenants_cleared', entity: 'review', entityId: review.id, detail: { removed, batchId } });
      }
      if (review.covenantIds.length === 0) {
        review.status = 'revision_cleared';
        review.resolvedAt = at;
        review.outcome = 'revision_cleared';
        review.note = '修订报表后全部违约消除，系统自动关闭';
        for (const t of this.s.trackers.filter((x) => x.periodEndWall === periodEndWall)) t.reviewId = null;
        this.store.audit({ actor, action: 'review.auto_close', entity: 'review', entityId: review.id, detail: { batchId } });
        review = null;
      } else {
        for (const b of breaches) {
          if (!review.assessmentIds.includes(b.id)) review.assessmentIds.push(b.id);
          if (b.alertId && !review.alertIds.includes(b.alertId)) review.alertIds.push(b.alertId);
          const f = this.s.freezes.find((x) => x.covenantId === b.covenantId && x.periodEndWall === periodEndWall && x.status === 'active');
          if (f && !review.freezeIds.includes(f.id)) review.freezeIds.push(f.id);
        }
      }
    } else if (breaches.length) {
      review = this._openReviewForBreaches({ batchId, breaches, periodEndWall, at, actor });
    }
    this.store.audit({
      actor, action: 'period.assess', entity: 'batch', entityId: batchId,
      detail: { periodEndWall, covenantCount: covenantIds.length, breaches: breaches.map((b) => b.covenantId), reviewId: review && review.id },
    });
    this.store.save();
    return { batchId, assessments, review, trackers };
  }

  _collectSources(periodEndWall) {
    // 报表：每个 docKey 只取最新 accepted 版本；重复件、已被取代件不进入计算。
    const latestByDoc = new Map();
    for (const r of this.s.reports.filter((x) => x.periodEndWall === periodEndWall && x.status === 'accepted')) {
      const k = r.rootId || r.id;
      const cur = latestByDoc.get(k);
      if (!cur || r.receivedAt > cur.receivedAt) latestByDoc.set(k, r);
    }
    // 佐证：只取 active。
    const evidences = this.s.evidences.filter((e) => e.periodEndWall === periodEndWall && e.status === 'active');
    return { reports: [...latestByDoc.values()], evidences };
  }

  _assessOne({ batchId, covenantId, version, tracker, periodEndWall, sources, at, actor, force }) {
    const reporting = this.s.facility.reportingCcy;
    const anchor = tracker.periodEndUtc; // fxPolicy=period_end
    const fxSnapshot = {};
    const usedSources = [];
    const evalLine = (entry) => {
      let rateInfo = { rateScaled: M.RATE_SCALE, native: true, id: null };
      if (entry.ccy !== reporting) {
        rateInfo = this.rateAt(entry.ccy, anchor);
        if (!rateInfo) throw new StationError('FX_MISSING', `期 ${periodEndWall} 缺少 ${entry.ccy}->${reporting} 的期末汇率`);
        fxSnapshot[entry.ccy] = { fxId: rateInfo.id, rate: rateInfo.rate, asOfWall: rateInfo.asOfWall };
      }
      const { converted, residual, rounded } = M.convert(entry.amountMinor, entry.ccy, reporting, rateInfo.rateScaled);
      return { converted, residual, rounded, ccy: entry.ccy, sourceCcyMinor: entry.amountMinor };
    };

    const metricOf = (metricId) => {
      const def = this.s.metricDefs.find((m) => m.id === metricId);
      if (!def) throw new StationError('METRIC_NOT_FOUND', metricId);
      let total = 0n;
      let found = 0;
      const breakdown = [];
      for (const key of def.keys) {
        for (const r of sources.reports) {
          for (const line of r.lines.filter((l) => l.key === key)) {
            const x = evalLine(line);
            total += x.converted;
            found += 1;
            breakdown.push({ key, source: 'report', sourceId: r.id, version: r.kind, fileName: r.fileName, ...x });
            usedSources.push({ type: 'report', id: r.id, docKey: r.docKey, fileName: r.fileName, kind: r.kind, sha256: r.sha256 });
          }
        }
        for (const e of sources.evidences.filter((x) => x.key === key)) {
          const x = evalLine(e);
          total += x.converted;
          found += 1;
          breakdown.push({ key, source: 'evidence', sourceId: e.id, ...x });
          usedSources.push({ type: 'evidence', id: e.id, providedBy: e.providedBy });
        }
      }
      return { metricId, total, found, breakdown };
    };

    const numParts = version.numerator.map(metricOf);
    const denParts = version.denominator ? version.denominator.map(metricOf) : null;
    const numerator = this._sumMetrics(numParts);
    const denominator = denParts ? this._sumMetrics(denParts) : null;
    const missingMetrics = [
      ...numParts.filter((p) => p.found === 0).map((p) => p.metricId),
      ...(denParts || []).filter((p) => p.found === 0).map((p) => p.metricId),
    ];

    const prevAssessment = tracker.latestAssessmentId ? this.s.assessments.find((a) => a.id === tracker.latestAssessmentId) : null;

    // 数据缺失（报表未到/未补发）：不定状态，不得按 0 误判违约；等补发后重评。
    if (missingMetrics.length) {
      const fingerprint = `missing:${version.versionId}:${missingMetrics.join(',')}`;
      if (!force && prevAssessment && prevAssessment.status === 'indeterminate' &&
          prevAssessment.fingerprint === fingerprint && prevAssessment.versionId === version.versionId) {
        return prevAssessment;
      }
      const id = this.store.nextId('asm');
      const rec = {
        id, batchId, covenantId, versionId: version.versionId, code: version.code, category: version.category,
        periodEndWall, periodEndUtc: tracker.periodEndUtc,
        versionNo: prevAssessment ? prevAssessment.versionNo + 1 : 1,
        supersedes: prevAssessment ? prevAssessment.id : null, supersededBy: null,
        status: 'indeterminate', reason: 'data_missing', missingMetrics,
        fingerprint,
        computed: null, threshold: version.threshold, operator: version.operator, warnBandPct: version.warnBandPct,
        distancePct: null, fxSnapshot,
        basis: { sources: this._dedupSources(usedSources), reportsLate: [], fxPolicy: version.fxPolicy, effectiveScope: version.impactScope || 'initial' },
        at,
      };
      this.s.assessments.push(rec);
      if (prevAssessment) prevAssessment.supersededBy = id;
      tracker.latestAssessmentId = rec.id;
      tracker.state = 'awaiting_data';
      return rec;
    }

    if (denominator === 0n) throw new StationError('DIV_ZERO', `${version.code} 分母为零，请核对口径`);
    const computed = denominator
      ? { kind: 'ratio', valueScaled: (numerator * M.RATE_SCALE) / denominator, numerator, denominator }
      : { kind: 'amount', valueMinor: numerator };

    const { status, distance } = this._classify(version, computed);

    const fingerprint = this._fingerprint({ version, computed, reporting, sources, fxSnapshot });

    // 输入（条款版本、报表/佐证版本、期末汇率）未变：复用既有评估，不再制造新版本或重复告警。
    // withdrawn_false_positive 同样作为终态复用——除非输入已更正（指纹自然变化），否则重跑不会复活违约。
    if (!force && prevAssessment &&
        prevAssessment.fingerprint === fingerprint && prevAssessment.versionId === version.versionId) {
      return prevAssessment;
    }

    const id = this.store.nextId('asm');
    const rec = {
      id, batchId, covenantId, versionId: version.versionId, code: version.code, category: version.category,
      periodEndWall, periodEndUtc: tracker.periodEndUtc,
      versionNo: prevAssessment ? prevAssessment.versionNo + 1 : 1,
      supersedes: prevAssessment ? prevAssessment.id : null, supersededBy: null,
      status, // compliant | near_threshold | breach | indeterminate
      fingerprint,
      computed: this._renderComputed(computed, reporting),
      threshold: version.threshold,
      operator: version.operator,
      warnBandPct: version.warnBandPct,
      distancePct: distance,
      fxSnapshot,
      basis: {
        sources: this._dedupSources(usedSources),
        reportsLate: sources.reports.filter((r) => r.receivedAt > tracker.graceEndUtc).map((r) => r.id),
        fxPolicy: version.fxPolicy,
        effectiveScope: version.impactScope || 'initial',
      },
      at,
    };
    this.s.assessments.push(rec);
    if (prevAssessment) prevAssessment.supersededBy = id;
    tracker.latestAssessmentId = id;
    tracker.state = status === 'breach' ? 'breached_pending_review' : `assessed_${status}`;

    // 接近阈值只提醒一次；breach 单独告警（同一约束同一期同类型告警去重）。
    const owner = this.s.parties[version.ownerPartyId];
    if (status === 'near_threshold') {
      this._alertOnce({ type: 'near_threshold', covenantId, periodEndUtc: tracker.periodEndUtc, severity: 'info', message: `${version.title} 接近阈值（差距 ${distance}%）` }, at, owner, rec);
    } else if (status === 'breach') {
      // 已转为违约：撤回早先的"接近阈值"提醒，避免两个口径的通知并存造成歧义。
      const nearKey = this._alertKey({ type: 'near_threshold', covenantId, periodEndUtc: tracker.periodEndUtc });
      const near = this.s.alerts.find((a) => a.key === nearKey && a.status === 'active');
      if (near) {
        near.status = 'withdrawn';
        near.withdrawnAt = at;
        near.withdrawnReason = '同一期已确认违约，接近提醒自动撤回';
      }
      const al = this._alertOnce({ type: 'breach', covenantId, periodEndUtc: tracker.periodEndUtc, severity: 'critical', message: `${version.title} 已确认违约：${rec.computed.display} vs 阈值 ${version.operator} ${version.threshold}` }, at, owner, rec);
      rec.alertId = al && al.id;
      tracker.cureEndUtc = at + version.cureDays * T.DAY_MS;
    } else if (status === 'compliant' && prevAssessment && prevAssessment.status === 'breach') {
      const confirmed = this.s.reviews.some((r) => r.status === 'confirmed' &&
        r.periodEndWall === periodEndWall && r.covenantIds.includes(covenantId));
      // 未经人工确认的违约，修订使指标恢复后自动撤回告警/解冻；
      // 已确认的违约属合同事实，冻结须走正式豁免流程，系统不得自动解除。
      if (!confirmed) {
        this._withdrawCovenantBreach(covenantId, tracker.periodEndUtc, at, '报表修订后指标恢复合规，违约告警自动撤回');
      }
    }
    return rec;
  }

  _withdrawCovenantBreach(covenantId, periodEndUtc, at, reason) {
    const breachKey = this._alertKey({ type: 'breach', covenantId, periodEndUtc });
    const alert = this.s.alerts.find((a) => a.key === breachKey && a.status === 'active');
    if (alert) {
      alert.status = 'withdrawn';
      alert.withdrawnAt = at;
      alert.withdrawnReason = reason;
    }
    for (const f of this.s.freezes.filter((x) => x.status === 'active' && x.covenantId === covenantId)) {
      f.status = 'lifted';
      f.liftedAt = at;
      f.liftReason = reason;
    }
  }

  _sumMetrics(list) {
    return list.reduce((acc, m) => acc + m.total, 0n);
  }

  _classify(version, computed) {
    const bandBps = BigInt(Math.round(version.warnBandPct * 100));
    if (computed.kind === 'ratio') {
      const thr = M.fixedToScaled(version.threshold);
      const v = computed.valueScaled;
      const ge = version.operator === 'ge';
      const breach = ge ? v < thr : v > thr;
      if (breach) {
        const distance = Number(((v > thr ? v - thr : thr - v) * 10000n) / thr) / 100;
        return { status: 'breach', distance: -distance };
      }
      const bandEdge = ge ? (thr * (10000n + bandBps)) / 10000n : (thr * (10000n - bandBps)) / 10000n;
      const near = ge ? v < bandEdge : v > bandEdge;
      if (near) {
        const distance = Number(((v - thr) * 10000n) / thr) / 100;
        return { status: 'near_threshold', distance };
      }
      return { status: 'compliant', distance: Number(((v - thr) * 10000n) / thr) / 100 };
    }
    // 金额：阈值按报告币种 major 解释
    const reporting = this.s.facility.reportingCcy;
    const thrMinor = M.toMinor(version.threshold, reporting);
    const v = computed.valueMinor;
    const ge = version.operator === 'ge';
    const breach = ge ? v < thrMinor : v > thrMinor;
    if (breach) return { status: 'breach', distance: null };
    const bandEdge = ge ? (thrMinor * (10000n + bandBps)) / 10000n : (thrMinor * (10000n - bandBps)) / 10000n;
    const near = ge ? v < bandEdge : v > bandEdge;
    return { status: near ? 'near_threshold' : 'compliant', distance: null };
  }

  _renderComputed(computed, reporting) {
    if (computed.kind === 'ratio') {
      const whole = computed.valueScaled / M.RATE_SCALE;
      const frac = (computed.valueScaled % M.RATE_SCALE).toString().padStart(8, '0').replace(/0+$/, '');
      return { kind: 'ratio', valueScaled: computed.valueScaled, display: `${whole}${frac ? '.' + frac : ''}` };
    }
    return { kind: 'amount', valueMinor: computed.valueMinor, ccy: reporting, display: `${M.toMajor(computed.valueMinor, reporting)} ${reporting}` };
  }

  _dedupSources(list) {
    const seen = new Set();
    return list.filter((x) => {
      const k = `${x.type}:${x.id}`;
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
  }

  // 评估指纹：条款版本 + 计算结果 + 实际使用的报表/佐证/汇率版本。任一输入变化才产生新评估版本。
  _fingerprint({ version, computed, sources, fxSnapshot }) {
    const crypto = require('crypto');
    const srcDesc = [
      ...sources.reports.map((r) => `R:${r.id}@${r.receivedAt}`),
      ...sources.evidences.map((e) => `E:${e.id}@${e.receivedAt}`),
    ].sort().join('|');
    const fxDesc = Object.keys(fxSnapshot).sort().map((k) => `${k}:${fxSnapshot[k].fxId}`).join('|');
    const valDesc = computed.kind === 'ratio'
      ? `ratio:${computed.valueScaled}`
      : `amount:${computed.valueMinor}`;
    return crypto.createHash('sha256')
      .update([version.versionId, valDesc, fxDesc, srcDesc].join('||'))
      .digest('hex').slice(0, 24);
  }

  // ---------- 告警与通知（去重 + 撤回） ----------

  _alertKey(a) {
    return `${a.type}:${a.covenantId}:${a.periodEndUtc}`;
  }

  _alert(payload, at) {
    const id = this.store.nextId('alt');
    const rec = { id, ...payload, key: this._alertKey(payload), status: 'active', createdAt: at, withdrawnAt: null, withdrawnBy: null, withdrawReason: null };
    this.s.alerts.push(rec);
    return rec;
  }

  _alertOnce(payload, at, owner, assessment) {
    const key = this._alertKey(payload);
    const existing = this.s.alerts.find((a) => a.key === key && a.status === 'active');
    if (existing) return existing;
    const rec = this._alert(payload, at);
    rec.assessmentId = assessment.id;
    this._notifyAlert(rec, owner, at);
    return rec;
  }

  _notify(type, tracker, owner, at, message, extra) {
    const id = this.store.nextId('ntf');
    const rec = { id, type, toPartyId: owner && owner.id, covenantId: tracker.covenantId, periodEndUtc: tracker.periodEndUtc, message, sentAt: at, status: 'sent', ...extra };
    this.s.notifications.push(rec);
    return rec;
  }

  _notifyAlert(alert, owner, at) {
    const dedupKey = `${alert.id}:${owner ? owner.id : 'none'}`;
    const id = this.store.nextId('ntf');
    const rec = { id, type: alert.type, alertId: alert.id, dedupKey, toPartyId: owner && owner.id, covenantId: alert.covenantId, periodEndUtc: alert.periodEndUtc, message: alert.message, sentAt: at, status: 'sent' };
    this.s.notifications.push(rec);
    return rec;
  }

  _ownerOf(covenantId) {
    const v = this.currentVersion(covenantId);
    return v ? this.s.parties[v.ownerPartyId] : null;
  }

  _cvLabel(covenantId) {
    const v = this.currentVersion(covenantId);
    return v ? `${v.code} ${v.title}` : covenantId;
  }

  // ---------- 违约：冻结提款 + 指定角色复核 ----------

  _openReviewForBreaches({ batchId, breaches, periodEndWall, at, actor }) {
    // 冻结：按条款类别冻结相关提款（同一批次多条违约 → 多类别冻结，共享一个复核任务）。
    const freezeIds = [];
    for (const b of breaches) {
      const existing = this.s.freezes.find((f) => f.covenantId === b.covenantId && f.periodEndWall === periodEndWall && f.status === 'active');
      if (existing) { freezeIds.push(existing.id); continue; }
      const fid = this.store.nextId('frz');
      const freeze = {
        id: fid, batchId, covenantId: b.covenantId, category: b.category, periodEndWall,
        status: 'active', reason: b.id, createdAt: at, liftedAt: null, liftedBy: null,
      };
      this.s.freezes.push(freeze);
      const tracker = this.s.trackers.find((t) => t.covenantId === b.covenantId && t.periodEndWall === b.periodEndUtc);
      if (tracker) tracker.freezeId = fid;
      freezeIds.push(fid);
    }

    const review = {
      id: this.store.nextId('rev'),
      batchId, periodEndWall,
      covenantIds: breaches.map((b) => b.covenantId),
      assessmentIds: breaches.map((b) => b.id),
      alertIds: breaches.map((b) => b.alertId).filter(Boolean),
      freezeIds,
      requiredRole: this.s.facility.reviewRole,
      assignedToPartyId: this._partyWithRole(this.s.facility.reviewRole),
      status: 'open',
      openedAt: at, openedBy: actor && actor.id,
      resolvedAt: null, resolvedBy: null, outcome: null, note: null,
    };
    this.s.reviews.push(review);
    for (const t of this.s.trackers.filter((x) => x.periodEndWall === periodEndWall && breaches.some((b) => b.covenantId === x.covenantId))) {
      t.reviewId = review.id;
      t.state = 'breached_pending_review';
    }
    const assignee = review.assignedToPartyId ? this.s.parties[review.assignedToPartyId] : null;
    const id0 = this.store.nextId('ntf');
    this.s.notifications.push({
      id: id0, type: 'review_required', reviewId: review.id,
      dedupKey: `review:${review.id}`,
      toPartyId: assignee && assignee.id,
      covenantId: null, periodEndUtc: null,
      message: `观察期 ${periodEndWall} 有 ${breaches.length} 条约束触发违约，相关提款已冻结，需 ${review.requiredRole} 复核`,
      sentAt: at, status: 'sent',
    });
    return review;
  }

  _partyWithRole(role) {
    const p = Object.values(this.s.parties).find((x) => x.role === role && x.active);
    return p ? p.id : null;
  }

  // 提款申请：存在与提款类别相关的 active 冻结即拦截（未声明类别时任何冻结都拦截）。
  requestDrawdown({ amount, ccy, category = null, purpose = '', atIso = null, actor }) {
    const at = atIso ? T.utc(atIso) : this.clock();
    const all = this.s.freezes.filter((f) => f.status === 'active');
    const blocking = category ? all.filter((f) => f.category === category) : all;
    if (blocking.length) {
      const rec = {
        id: this.store.nextId('drw'), amountMinor: M.toMinor(amount, ccy), ccy, category, purpose,
        status: 'blocked', blockedBy: blocking.map((f) => f.id), requestedAt: at, requestedBy: actor && actor.id,
      };
      this.s.drawdowns.push(rec);
      this.store.audit({ actor, action: 'drawdown.blocked', entity: 'drawdown', entityId: rec.id, detail: { freezes: blocking.map((f) => f.id), category } });
      this.store.save();
      return rec;
    }
    const rec = {
      id: this.store.nextId('drw'), amountMinor: M.toMinor(amount, ccy), ccy, category, purpose,
      status: 'approved', requestedAt: at, requestedBy: actor && actor.id,
    };
    this.s.drawdowns.push(rec);
    this.store.audit({ actor, action: 'drawdown.approve', entity: 'drawdown', entityId: rec.id, detail: { amount, ccy, category } });
    this.store.save();
    return rec;
  }

  /**
   * 复核结论：
   *  - confirmed：违约成立，维持冻结（可另行豁免），关闭复核
   *  - false_positive：误报撤回 —— 告警置 withdrawn、冻结 lifted、相关评估以新版本标记 withdrawn_false_positive，审计可解释
   */
  resolveReview(reviewId, { outcome, note = '', atIso = null, actor }) {
    const review = this.s.reviews.find((r) => r.id === reviewId);
    if (!review) throw new StationError('REVIEW_NOT_FOUND', reviewId);
    if (review.status !== 'open') throw new StationError('REVIEW_CLOSED', reviewId);
    if (!actor || actor.role !== review.requiredRole) {
      throw new StationError('ROLE_REQUIRED', `该复核必须由 ${review.requiredRole} 完成`);
    }
    const at = atIso ? T.utc(atIso) : this.clock();

    if (outcome === 'false_positive') {
      for (const aid of review.alertIds) {
        const alert = this.s.alerts.find((a) => a.id === aid);
        if (alert && alert.status === 'active') {
          alert.status = 'withdrawn';
          alert.withdrawnAt = at;
          alert.withdrawnBy = actor.id;
          alert.withdrawReason = note || '复核确认误报';
        }
      }
      for (const fid of review.freezeIds) {
        const f = this.s.freezes.find((x) => x.id === fid);
        if (f && f.status === 'active') {
          f.status = 'lifted';
          f.liftedAt = at;
          f.liftedBy = actor.id;
          f.liftReason = 'false_positive';
        }
      }
      // 以新版本评估取代误报结果，原评估保留不删改（状态加注释，不改判定数字）。
      for (const asmId of review.assessmentIds) {
        const prev = this.s.assessments.find((a) => a.id === asmId);
        if (!prev) continue;
        const corrected = {
          ...prev,
          id: this.store.nextId('asm'),
          versionNo: prev.versionNo + 1,
          supersedes: prev.id,
          supersededBy: null,
          status: 'withdrawn_false_positive',
          withdrawnReason: note || '人工复核确认误报',
          withdrawnBy: actor.id,
          withdrawnAt: at,
          reviewId,
          at,
        };
        delete corrected.alertId;
        prev.supersededBy = corrected.id;
        this.s.assessments.push(corrected);
        const tracker = this.s.trackers.find((t) => t.covenantId === prev.covenantId && t.periodEndUtc === prev.periodEndUtc);
        if (tracker) {
          tracker.latestAssessmentId = corrected.id;
          tracker.state = 'assessed_compliant_fp_withdrawn';
          tracker.freezeId = null;
        }
      }
    }

    review.status = outcome === 'false_positive' ? 'false_positive' : 'confirmed';
    review.resolvedAt = at;
    review.resolvedBy = actor.id;
    review.outcome = outcome;
    review.note = note;
    for (const t of this.s.trackers.filter((x) => x.reviewId === reviewId)) {
      if (outcome === 'false_positive') t.reviewId = null;
      else t.state = 'breach_confirmed';
    }
    this.store.audit({
      actor, action: 'review.resolve', entity: 'review', entityId: reviewId,
      detail: { outcome, note, alertIds: review.alertIds, freezeIds: review.freezeIds },
    });
    this.store.save();
    return review;
  }

  // 隔日重启后依然回答两件事：还有哪些未决复核、下一次报表截止是什么时候。
  pendingReviews(nowIso = null) {
    const now = nowIso ? T.utc(nowIso) : this.clock();
    const openReviews = this.s.reviews.filter((r) => r.status === 'open');
    const deadlines = this.s.trackers
      .map((t) => ({
        trackerId: t.id, covenantId: t.covenantId, periodEndWall: t.periodEndWall,
        dueUtc: t.dueUtc, graceEndUtc: t.graceEndUtc,
        state: t.state, assessed: Boolean(t.latestAssessmentId),
        overdue: now > t.graceEndUtc && !t.latestAssessmentId,
      }))
      .sort((a, b) => a.dueUtc - b.dueUtc);
    return { asOf: new Date(now).toISOString(), openReviews, deadlines };
  }

  _assertActor(actor, roles) {
    if (!actor || !roles.includes(actor.role)) throw new StationError('FORBIDDEN', `需要角色: ${roles.join('/')}`);
  }

  // ---------- 查询 ----------

  explain(assessmentId) {
    const a = this.s.assessments.find((x) => x.id === assessmentId);
    if (!a) throw new StationError('NOT_FOUND', assessmentId);
    return {
      assessment: a,
      covenantVersion: this.s.covenants.find((c) => c.versionId === a.versionId),
      audit: this.store.auditTrail({ entity: 'covenant', entityId: a.covenantId }),
    };
  }

  status() {
    return {
      facility: this.s.facility,
      parties: this.s.parties,
      covenants: this.s.covenants,
      trackers: this.s.trackers,
      alerts: this.s.alerts,
      activeFreezes: this.s.freezes.filter((f) => f.status === 'active'),
      openReviews: this.s.reviews.filter((r) => r.status === 'open'),
    };
  }
}

Station.ROLES = ROLES;
Station.StationError = StationError;

module.exports = { Station };
