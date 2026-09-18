'use strict';

// 零依赖 HTTP 入口：把观察站能力暴露为 JSON API，状态持久化到 DATA_FILE（默认 data/state.json）。
// 调用方通过 x-user-id 头表明身份（角色以登记为准），所有写操作自动进入审计日志。
const http = require('http');
const { Station } = require('./station');

const DATA_FILE = process.env.DATA_FILE || `${__dirname}/../data/state.json`;
const PORT = process.env.PORT || '8080';

const station = Station.create(DATA_FILE);

function json(res, code, body) {
  const buf = Buffer.from(JSON.stringify(body, (_k, v) => (typeof v === 'bigint' ? v.toString() : v), 2));
  res.writeHead(code, { 'content-type': 'application/json; charset=utf-8' });
  res.end(buf);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let raw = '';
    req.on('data', (c) => { raw += c; if (raw.length > 5_000_000) reject(new Error('body too large')); });
    req.on('end', () => { try { resolve(raw ? JSON.parse(raw) : {}); } catch (e) { reject(e); } });
  });
}

function actorOf(req) {
  const id = req.headers['x-user-id'];
  if (!id) return null;
  const p = station.s.parties[id];
  return p ? { id: p.id, name: p.name, role: p.role } : { id, role: 'unknown' };
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url, 'http://localhost');
    const p = url.pathname;
    const actor = actorOf(req);
    const body = ['POST', 'PUT', 'PATCH'].includes(req.method) ? await readBody(req) : {};
    if (actor) body.actor = actor;

    if (req.method === 'GET' && p === '/health') return json(res, 200, { status: 'ok' });
    if (req.method === 'GET' && p === '/status') return json(res, 200, station.status());
    if (req.method === 'GET' && p === '/pending') return json(res, 200, station.pendingReviews());

    if (req.method === 'POST' && p === '/admin/setup') return json(res, 200, station.setupFacility(body));
    if (req.method === 'POST' && p === '/parties') return json(res, 200, station.registerParty(body));
    if (req.method === 'POST' && p === '/metrics') return json(res, 200, station.defineMetric(body));
    if (req.method === 'POST' && p === '/fx') return json(res, 200, station.ingestRate(body));
    if (req.method === 'POST' && p === '/covenants') return json(res, 200, station.issueCovenant(body, actor));
    if (req.method === 'POST' && p === '/periods/open') return json(res, 200, station.openPeriod(body.periodEndWall, actor));
    if (req.method === 'POST' && p === '/reports') return json(res, 200, station.ingestReport(body));
    if (req.method === 'POST' && p === '/evidence') return json(res, 200, station.ingestEvidence(body));
    if (req.method === 'POST' && p === '/drawdowns') return json(res, 200, station.requestDrawdown(body));
    if (req.method === 'POST' && p === '/tick') return json(res, 200, { fired: station.tick(body.atIso, actor) });

    let m;
    if (req.method === 'POST' && (m = p.match(/^\/periods\/(\d{4}-\d{2}-\d{2})\/assess$/))) {
      return json(res, 200, station.assessPeriod(m[1], { atIso: body.atIso, only: body.only, actor }));
    }
    if (req.method === 'POST' && (m = p.match(/^\/covenants\/([^/]+)\/amend$/))) {
      return json(res, 200, station.amendCovenant(m[1], body, actor));
    }
    if (req.method === 'POST' && (m = p.match(/^\/reviews\/([^/]+)\/resolve$/))) {
      return json(res, 200, station.resolveReview(m[1], { outcome: body.outcome, note: body.note, atIso: body.atIso, actor }));
    }
    if (req.method === 'GET' && (m = p.match(/^\/assessments\/([^/]+)\/explain$/))) {
      return json(res, 200, station.explain(m[1]));
    }
    return json(res, 404, { error: 'not_found', path: p });
  } catch (e) {
    return json(res, e.code && e.code === e.code.toUpperCase() ? 422 : 500, {
      error: e.code || 'internal', message: e.message,
    });
  }
});

if (require.main === module) {
  server.listen(PORT, () => {
    console.log(`融资契约观察站已启动: http://localhost:${PORT}  数据文件: ${DATA_FILE}`);
  });
}

module.exports = { server };
