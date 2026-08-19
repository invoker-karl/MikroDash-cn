'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { RStream } = require('node-routeros/dist/RStream');
const {
  AuthoritativeSnapshotProbe,
  classifyRStreamPacket,
  classifySnapshotError,
} = require('../src/collectors/rstreamSnapshot');

test('installed RStream emits [] after an idle interval, not from RouterOS data', (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const stream = new RStream({}, ['/table/print', '=interval=1'], null);
  const packets = [];
  stream.on('data', packet => packets.push(packet));
  stream.prepareDebounceEmptyData();
  // This is the order used by RStream's channel handler: arm/reset the idle
  // timer, then deliver the real RouterOS row.
  stream.debounceSendingEmptyData.run();
  stream.onStream({ '.id': '*1' });
  t.mock.timers.tick(1299);
  assert.deepEqual(packets, [{ '.id': '*1' }]);
  t.mock.timers.tick(1);
  assert.deepEqual(packets, [{ '.id': '*1' }, []]);
  assert.equal(classifyRStreamPacket(packets[1]).kind, 'idle');
  stream.debounceSendingEmptyData.cancel();
});

test('snapshot probe coalesces idle, and real rows invalidate a late result', async () => {
  let resolveRead;
  let reads = 0;
  const applied = [];
  const probe = new AuthoritativeSnapshotProbe({
    cooldownMs: 0,
    read: () => { reads++; return new Promise(resolve => { resolveRead = resolve; }); },
    apply: rows => applied.push(rows),
  });
  probe.onIdle();
  probe.onIdle();
  await Promise.resolve();
  assert.equal(reads, 1);
  probe.noteRealRow();
  resolveRead([]);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(applied, [], 'a real stream row makes the older probe stale');
});

test('snapshot probe invalidation rejects stop/reconnect completions and preserves errors', async () => {
  let resolveRead;
  const applied = [];
  const errors = [];
  const probe = new AuthoritativeSnapshotProbe({
    cooldownMs: 0,
    read: () => new Promise(resolve => { resolveRead = resolve; }),
    apply: rows => applied.push(rows),
    onError: (_error, classification) => errors.push(classification),
  });
  probe.onIdle();
  await Promise.resolve();
  probe.invalidate();
  resolveRead([]);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(applied, []);
  assert.deepEqual(errors, []);
  assert.equal(classifySnapshotError(new Error('permission denied')).kind, 'permission');
  assert.equal(classifySnapshotError(new Error('unknown command')).kind, 'unsupported');
  assert.equal(classifySnapshotError(new Error('timeout')).kind, 'transient');
});

test('a real row cancels an idle probe queued behind cooldown', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let reads = 0;
  const probe = new AuthoritativeSnapshotProbe({
    cooldownMs: 1000,
    read: async () => { reads++; return []; },
    apply: () => assert.fail('a pre-row idle must not apply after the real row'),
  });
  probe._lastStart = Date.now();
  probe.onIdle();
  probe.noteRealRow();
  t.mock.timers.tick(1000);
  await Promise.resolve();
  assert.equal(reads, 0);
});
