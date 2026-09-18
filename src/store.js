'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');

// 纯文件持久化：每次状态变更原子写盘（tmp + rename），进程隔日重启后状态可完整恢复。
// 金额与定点汇率是 BigInt，JSON 不支持，用 {"$bigint":"..."} 包装序列化。

const EMPTY_STATE = () => ({
  schemaVersion: 1,
  seq: 0,
  facility: null,
  parties: {},
  covenants: [],     // 条款版本链：同一 covenantId 的每次修订新增一行
  fxRates: [],      // 汇率按口径/生效时点不可变追加
  metricDefs: [],
  reports: [],      // 银行报表版本链（补发/修订/重复上传）
  evidences: [],    // 人工佐证与更正
  values: [],       // 指标量值（挂报表或佐证，带取代链）
  trackers: [],     // 每条款 × 每观察期的跟踪状态
  assessments: [],  // 每期计算结果（不可变，新结果以版本链取代旧结果）
  alerts: [],       // 告警（含撤回状态）
  notifications: [],// 送达记录（去重）
  freezes: [],      // 提款冻结
  drawdowns: [],    // 提款申请
  reviews: [],      // 复核任务
  batches: [],      // 同一次评估批次（如两条约束同时触发）
  auditLog: [],
});

function reviver(_key, v) {
  if (v && typeof v === 'object' && typeof v.$bigint === 'string') return BigInt(v.$bigint);
  return v;
}

function replacer(_key, v) {
  if (typeof v === 'bigint') return { $bigint: v.toString() };
  return v;
}

class Store {
  constructor(filePath, { clock = () => Date.now() } = {}) {
    this.filePath = filePath;
    this.clock = clock;
    this._dirty = false;
    if (filePath && fs.existsSync(filePath)) {
      this.state = JSON.parse(fs.readFileSync(filePath, 'utf8'), reviver);
    } else {
      this.state = EMPTY_STATE();
      this.save();
    }
  }

  static empty({ clock } = {}) {
    const s = new Store(null, { clock });
    s._dirty = true;
    return s;
  }

  nextId(prefix) {
    this.state.seq += 1;
    return `${prefix}_${String(this.state.seq).padStart(4, '0')}`;
  }

  save() {
    if (!this.filePath) return;
    const dir = path.dirname(this.filePath);
    fs.mkdirSync(dir, { recursive: true });
    const tmp = path.join(dir, `.${path.basename(this.filePath)}.${process.pid}.${crypto.randomBytes(3).toString('hex')}.tmp`);
    fs.writeFileSync(tmp, JSON.stringify(this.state, replacer, 2));
    fs.renameSync(tmp, this.filePath);
  }

  // 隔日重启：从磁盘重新装载，返回新的 Store（模拟新进程）。
  static reopen(filePath, opts) {
    return new Store(filePath, opts);
  }

  hashBuffer(buf) {
    return crypto.createHash('sha256').update(buf).digest('hex');
  }

  audit({ actor, action, entity, entityId = null, detail = {}, at = null }) {
    const entry = {
      seq: this.state.auditLog.length + 1,
      at: at === null ? this.clock() : at,
      actor: actor ? { id: actor.id, name: actor.name || null, role: actor.role } : null,
      action,
      entity,
      entityId,
      detail,
    };
    this.state.auditLog.push(entry);
    return entry;
  }

  auditTrail({ entity, entityId } = {}) {
    return this.state.auditLog.filter((e) =>
      (!entity || e.entity === entity) && (!entityId || e.entityId === entityId));
  }
}

module.exports = { Store, EMPTY_STATE };
