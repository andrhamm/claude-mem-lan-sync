#!/usr/bin/env node
// Validates real hub responses using claude-mem's own validation rules.
//
// These checks are transcribed from the client's validators in
// worker-service.cjs (De, xf, fi/canon, Sa, SF, validatePushResponse). Every
// other test in this repository asserts against our Go reimplementation of the
// protocol; this one asserts against the rules the client actually applies, so
// it catches a misunderstanding that our own code and tests share.
//
// Usage: node validate.js <hub-url> <user-id> <token>

const crypto = require('crypto');

const [, , hubURL, userID, token] = process.argv;
if (!hubURL || !userID || !token) {
  console.error('usage: validate.js <hub-url> <user-id> <token>');
  process.exit(2);
}

const DECIMAL = /^(?:0|[1-9][0-9]*)$/;
const UINT64_MAX = 18446744073709551615n;
const DIGEST = /^[A-Za-z0-9_-]{43}$/;
const BODY_KEYS = [
  'body_schema_version', 'deleted', 'deleted_at', 'entity_rev', 'id', 'kind',
  'mutation', 'origin_device_id', 'origin_local_id', 'payload',
  'payload_schema_version', 'payload_sha256',
];

let failures = 0;
const pass = (m) => console.log('PASS', m);
const fail = (m) => { console.log('FAIL', m); failures++; };

// De: the client's decimal-string check. A JSON number throws here, which is
// what wedges a client mid-page.
function dec(v, { positive = false } = {}) {
  if (typeof v !== 'string' || !DECIMAL.test(v)) {
    throw new Error('decimal values must be unsigned base-10 strings without leading zeroes');
  }
  if (BigInt(v) > UINT64_MAX) throw new Error('decimal value exceeds uint64');
  if (positive && v === '0') throw new Error('decimal value must be positive');
  return v;
}

// fi: canonical JSON — sorted keys, plain objects.
const canon = (v) => JSON.stringify(sortKeys(v));
function sortKeys(t) {
  if (t === null || typeof t !== 'object') return t;
  if (Array.isArray(t)) return t.map(sortKeys);
  const out = {};
  for (const k of Object.keys(t).sort()) out[k] = sortKeys(t[k]);
  return out;
}

const digest = (s) => crypto.createHash('sha256').update(s, 'utf8').digest('base64url');

// xf: operation wrapper validation, including the canonical-form re-check that
// makes byte preservation load-bearing.
function checkOp(op) {
  const keys = Object.keys(op).sort();
  if (keys.length !== 2 || keys[0] !== 'body' || keys[1] !== 'operation_sha256') {
    throw new Error('operation wrapper must contain only body and operation_sha256');
  }
  if (typeof op.body !== 'string' || op.body.length === 0) throw new Error('body must be a non-empty string');
  if (!DIGEST.test(op.operation_sha256)) throw new Error('operation_sha256 must be a base64url digest');
  if (digest(op.body) !== op.operation_sha256) throw new Error('operation_sha256 mismatch');

  const parsed = JSON.parse(op.body);
  if (canon(parsed) !== op.body) throw new Error('body is not canonical JSON');

  const got = Object.keys(parsed).sort();
  if (got.length !== BODY_KEYS.length || got.some((k, i) => k !== BODY_KEYS[i])) {
    throw new Error('body must carry exactly the twelve canonical keys');
  }
  return parsed;
}

const headers = {
  Authorization: `Bearer ${token}`,
  'X-User-Id': userID,
  'X-Device-Id': 'clientvalidate',
  'X-Device-Name': 'clientvalidate',
};

async function main() {
  // A body built the way the client builds one: JS canonicalisation, raw
  // non-ASCII, and the characters encoding/json would silently re-escape.
  const payload = { project: 'validate', text: 'markup < > & snowman ☃ emoji 😀 quote "q"' };
  const body = canon({
    body_schema_version: 1, deleted: false, deleted_at: null, entity_rev: '1',
    id: 'observation:clientvalidate', kind: 'observation', mutation: null,
    origin_device_id: 'clientvalidate', origin_local_id: '1', payload,
    payload_schema_version: 2, payload_sha256: digest(canon(payload)),
  });
  const push = { protocol_version: 2, ops: [{ body, operation_sha256: digest(body) }] };

  // --- POST /v1/sync/ops, as validatePushResponse checks it
  const opsRes = await fetch(`${hubURL}/v1/sync/ops`, {
    method: 'POST',
    headers: { ...headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(push),
  });
  if (!opsRes.ok) { fail(`/v1/sync/ops returned ${opsRes.status}`); return; }
  const acked = await opsRes.json();
  try {
    if (BigInt(acked.head_seq) > BigInt(acked.projected_seq)) {
      throw new Error('checkpoint order requires head_seq <= projected_seq');
    }
    if (acked.acked.length !== push.ops.length) throw new Error('acknowledgment multiplicity mismatch');
    for (const a of acked.acked) {
      for (const f of ['id', 'kind', 'entity_rev', 'operation_sha256', 'seq']) {
        if (typeof a[f] !== 'string') throw new Error(`malformed ack field ${f}`);
      }
      if (!('origin_local_id' in a)) throw new Error('origin_local_id must be present');
      if (a.origin_local_id !== null && typeof a.origin_local_id !== 'string') {
        throw new Error('origin_local_id must be string or null');
      }
      dec(a.entity_rev, { positive: true });
      dec(a.seq, { positive: true });
      if (BigInt(a.seq) > BigInt(acked.head_seq)) throw new Error('acknowledgment seq exceeds head_seq');
      if (a.operation_sha256 !== push.ops[0].operation_sha256) {
        throw new Error('ack must echo the digest the client sent');
      }
    }
    pass('/v1/sync/ops');
  } catch (e) { fail(`/v1/sync/ops: ${e.message}`); }

  // --- GET /v1/sync/status, as probeHubStatus checks it
  const statusRes = await fetch(`${hubURL}/v1/sync/status`, { headers });
  const status = await statusRes.json();
  try {
    if (status.protocol_version !== 2) throw new Error('response requires protocol_version 2');
    dec(status.epoch, { positive: true });
    dec(status.head_seq);
    dec(status.projected_seq);
    if (BigInt(status.projected_seq) > BigInt(status.head_seq)) {
      throw new Error('projected_seq exceeds head_seq');
    }
    if (statusRes.headers.get('x-sync-mode') !== 'poll') {
      throw new Error('a hub without /v1/sync/ws must send X-Sync-Mode: poll');
    }
    pass('/v1/sync/status');
  } catch (e) { fail(`/v1/sync/status: ${e.message}`); }

  // --- GET /v1/sync/changes, as pullCycle, SF and tee check it
  const changesRes = await fetch(`${hubURL}/v1/sync/changes?since=0&limit=500`, { headers });
  const changes = await changesRes.json();
  try {
    if (changes.protocol_version !== 2) throw new Error('response requires protocol_version 2');
    if (!Array.isArray(changes.ops)) throw new Error('ops must be an array');
    if (typeof changes.more !== 'boolean') throw new Error('more must be a boolean');
    dec(changes.epoch);
    dec(changes.head_seq);

    let cursor = 0n;
    let sawOurs = false;
    for (const op of changes.ops) {
      const seq = dec(op.seq, { positive: true });
      if (op.server_ts !== undefined) dec(op.server_ts); // a JSON number throws
      if (BigInt(seq) !== cursor + 1n) {
        throw new Error(`sequence gap: expected ${cursor + 1n}, got ${seq}`);
      }
      cursor = BigInt(seq);

      const parsed = checkOp({ body: op.body, operation_sha256: op.operation_sha256 });
      if (parsed.id === 'observation:clientvalidate') {
        sawOurs = true;
        if (op.body !== body) throw new Error('body did not survive byte-for-byte');
        if (!parsed.payload.text.includes('☃') || !parsed.payload.text.includes('😀')
            || !parsed.payload.text.includes('<')) {
          throw new Error('content was altered in transit');
        }
      }
    }
    if (!sawOurs) throw new Error('the pushed operation did not come back');
    pass(`/v1/sync/changes (${changes.ops.length} ops, contiguity and canonical bodies verified)`);
  } catch (e) { fail(`/v1/sync/changes: ${e.message}`); }

  if (failures > 0) {
    console.log(`\n${failures} check(s) failed against claude-mem's own validation rules`);
    process.exit(1);
  }
  console.log('\nAll responses satisfy claude-mem 13.15.0 validation rules');
}

main().catch((e) => { console.error(e); process.exit(1); });
