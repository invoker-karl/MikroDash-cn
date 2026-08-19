'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { Server } = require('socket.io');
const { io: connectClient } = require('socket.io-client');
const { JSDOM } = require('jsdom');

const TrafficCollector = require('../src/collectors/traffic');
const TopTalkersCollector = require('../src/collectors/talkers');
const DhcpNetworksCollector = require('../src/collectors/dhcpNetworks');
const { computeHealthStatus } = require('../src/health');
const { mayPromote } = require('../scripts/release-guard');

function wait(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }

function collectorIo(events, clients = 1) {
  const target = {
    engine: { clientsCount: clients }, on() {},
    emit(ev, data) { events.push({ ev, data }); },
    to() { return target; },
  };
  return target;
}

function streamRos() {
  const ros = new EventEmitter();
  ros.connected = true;
  ros.streams = [];
  ros.stream = (cmd, params) => {
    const stream = new EventEmitter();
    stream.stop = () => {};
    ros.streams.push({ cmd, params, stream });
    return stream;
  };
  return ros;
}

test('Top Talkers commits non-empty to empty transitions in stream and poll modes', async () => {
  const events = [];
  const ros = streamRos();
  const collector = new TopTalkersCollector({
    ros, io: collectorIo(events), pollMs: 1000, state: {}, topN: 5, streamMode: true,
  });
  collector.start();
  const stream = ros.streams[0].stream;
  stream.emit('data', { name: 'phone', 'mac-address': 'AA', 'rate-up': '1000', 'rate-down': '2000' });
  await wait(330);
  stream.emit('data', []);
  await wait(330);
  const updates = events.filter(event => event.ev === 'talkers:update');
  assert.equal(updates.length, 2);
  assert.equal(updates[0].data.devices.length, 1);
  assert.deepEqual(updates[1].data.devices, []);
  assert.equal(updates[1].data.unavailable, false);
  collector.stop();

  const pollEvents = [];
  const pollRos = new EventEmitter();
  pollRos.connected = true;
  let rows = [{ name: 'phone', 'mac-address': 'AA', 'rate-up': '1000', 'rate-down': '2000' }];
  pollRos.write = async () => rows;
  const polled = new TopTalkersCollector({
    ros: pollRos, io: collectorIo(pollEvents), pollMs: 1000, state: {}, topN: 5, streamMode: false,
  });
  await polled._pollTalkersOnce();
  rows = [];
  await polled._pollTalkersOnce();
  assert.deepEqual(pollEvents.filter(event => event.ev === 'talkers:update').at(-1).data.devices, []);
  polled.stop();
});

test('Top Talkers unavailable has a distinct safe payload', () => {
  const events = [];
  const ros = streamRos();
  const collector = new TopTalkersCollector({ ros, io: collectorIo(events), pollMs: 1000, state: {} });
  collector.start();
  collector._stream.emit('error', new Error('unknown command'));
  const payload = events.find(event => event.ev === 'talkers:update').data;
  assert.equal(payload.unavailable, true);
  assert.equal(payload.reason, 'Kid Control is unavailable');
  assert.deepEqual(payload.devices, []);
  collector.stop();
});

test('dashboard DOM clears talkers and distinguishes unavailable from a legal empty table', () => {
  const app = fs.readFileSync(path.join(__dirname, '..', 'public', 'app.js'), 'utf8');
  const source = app.slice(app.indexOf("socket.on('talkers:update'"), app.indexOf('// ── Interface Status'));
  const dom = new JSDOM('<table><tbody id="talkersTable"><tr><td>stale</td></tr></tbody></table>');
  const handlers = {};
  const context = {
    socket: { on(name, handler) { handlers[name] = handler; } },
    talkersTable: dom.window.document.getElementById('talkersTable'),
    lastTalkers: [{ mac: 'old' }], esc: value => String(value), fmtMbps: String,
  };
  vm.runInNewContext(source, context);
  handlers['talkers:update']({ devices: [] });
  assert.match(context.talkersTable.textContent, /No devices/);
  assert.doesNotMatch(context.talkersTable.textContent, /stale/);
  handlers['talkers:update']({ devices: [], unavailable: true });
  assert.match(context.talkersTable.textContent, /Kid Control is unavailable/);
});

test('Detect Internet rejection is distinct from a successful empty result and preserves other DHCP data', async () => {
  const events = [];
  const ros = new EventEmitter();
  ros.connected = true;
  ros.write = async (cmd) => {
    if (cmd.includes('detect-internet')) throw new Error('not permitted: secret detail');
    if (cmd.includes('network')) return [{ address: '192.168.1.0/24', gateway: '192.168.1.1' }];
    return [];
  };
  const collector = new DhcpNetworksCollector({
    ros, io: collectorIo(events), pollMs: 15000,
    dhcpLeases: { getAllLeaseIPs: () => [] }, state: {}, wanIface: 'wan',
  });
  await collector._fetchOnce();
  assert.equal(collector.lastPayload.networks.length, 1);
  assert.equal(collector.lastPayload.internetStatus.available, false);
  assert.equal(collector.lastPayload.internetStatus.stale, false);
  assert.equal(collector.lastPayload.internetStatus.reason, 'Detect Internet is unavailable');
  assert.doesNotMatch(JSON.stringify(collector.lastPayload), /secret detail/);

  ros.write = async (cmd) => cmd.includes('detect-internet') ? [] : [];
  await collector._fetchOnce();
  assert.equal(collector.lastPayload.internetStatus.available, true);
  assert.equal(collector.lastPayload.internetStatus.stale, false);
  assert.deepEqual(collector.lastPayload.internetIfaces, []);

  ros.write = async (cmd) => {
    if (cmd.includes('detect-internet')) throw new Error('temporary failure');
    return [];
  };
  await collector._fetchOnce();
  assert.equal(collector.lastPayload.internetStatus.available, false);
  assert.equal(collector.lastPayload.internetStatus.stale, true);
  assert.ok(Number.isFinite(collector.lastPayload.internetStatus.updatedAt));
});

test('interface picker preserves a non-disabled down interface and error is not success-empty', () => {
  const app = fs.readFileSync(path.join(__dirname, '..', 'public', 'app.js'), 'utf8');
  const start = app.indexOf('function _rebuildIfaceSelect');
  const end = app.indexOf("ifaceSelect.addEventListener('change'", start);
  const source = app.slice(start, end);
  const dom = new JSDOM('<select id="iface"></select>');
  const handlers = {};
  const emitted = [];
  const context = {
    document: dom.window.document,
    ifaceSelect: dom.window.document.getElementById('iface'),
    currentIf: 'wan', _ifaceSelectKey: '',
    socket: {
      on(name, handler) { handlers[name] = handler; },
      emit(name, payload) { emitted.push({ name, payload }); },
    },
    console: { warn() {} },
  };
  vm.runInNewContext(source, context);
  handlers['interfaces:list']({
    ok: true, defaultIf: 'wan',
    interfaces: [{ name: 'wan', running: false, disabled: false }, { name: 'lan', running: true, disabled: false }],
  });
  assert.equal(context.ifaceSelect.value, 'wan');
  assert.match(context.ifaceSelect.options[0].textContent, /down/);
  assert.equal(emitted.length, 0, 'link-down alone is not a fallback');

  handlers['interfaces:error']({ ok: false, reason: 'unavailable' });
  assert.equal(context.ifaceSelect.options.length, 1);
  assert.equal(context.ifaceSelect.options[0].textContent, 'Interface list unavailable');
  assert.equal(context.ifaceSelect.options[0].disabled, true);

  context.currentIf = 'missing';
  handlers['interfaces:list']({
    ok: true, defaultIf: 'missing', interfaces: [{ name: 'lan', running: false, disabled: false }],
  });
  assert.equal(context.ifaceSelect.value, 'lan');
  assert.equal(emitted.at(-1).payload.ifName, 'lan');

  // A router switch can keep the same option key while resetting currentIf.
  // Selection reconciliation must still run in that case.
  context.currentIf = 'missing-again';
  const before = emitted.length;
  handlers['interfaces:list']({
    ok: true, defaultIf: 'missing-again', interfaces: [{ name: 'lan', running: false, disabled: false }],
  });
  assert.equal(emitted.length, before + 1);
  assert.equal(emitted.at(-1).payload.ifName, 'lan');
});

test('invalid configured default keeps health at 503 with and without browser fallback traffic', () => {
  const now = Date.now();
  const state = { lastTrafficTs: now, trafficConfigValid: false };
  for (const requiredCollectors of [['traffic'], ['traffic', 'system', 'ifstatus']]) {
    const result = computeHealthStatus({
      startupReady: true, rosConnected: true, state: {
        ...state, lastSystemTs: now, lastIfStatusTs: now,
      }, now, requiredCollectors,
    });
    assert.equal(result.statusCode, 503);
    assert.ok(result.stale.includes('traffic-config'));
  }
  assert.equal(computeHealthStatus({
    startupReady: true, rosConnected: true,
    state: { ...state, trafficConfigValid: true }, now,
  }).statusCode, 200);
});

test('traffic health logging follows the combined config and stream state', () => {
  const ros = streamRos();
  const events = [];
  const collector = new TrafficCollector({
    ros, io: collectorIo(events), defaultIf: 'missing', historyMinutes: 1, state: {},
  });
  const messages = [];
  const originalWarn = console.warn;
  console.warn = (...args) => messages.push(args.join(' '));
  try {
    collector.setAvailableInterfaces(['lan']);
    messages.length = 0;
    collector._reportHealth(false);
  } finally {
    console.warn = originalWarn;
    collector.stop();
  }
  assert.match(messages.at(-1), /stream degraded/);
  assert.doesNotMatch(messages.at(-1), /recovered/);
});

test('initial state explicitly replays healthy or degraded traffic and connections health', () => {
  const source = fs.readFileSync(path.join(__dirname, '..', 'src', 'index.js'), 'utf8');
  const start = source.indexOf('async function sendInitialState');
  const body = source.slice(start, source.indexOf('// ── Socket.IO', start));
  assert.match(body, /\['traffic', s\.traffic\]/);
  assert.match(body, /\['connections', s\.conns\]/);
  assert.match(body, /degraded: false, restarts: 0/);
  assert.match(source, /function refreshAndBroadcastSessionInterfaces/);
  assert.match(source, /_ifacesRetryTimer = setTimeout/);
  assert.match(source, /session\._ifacesFetch = null/);
  const app = fs.readFileSync(path.join(__dirname, '..', 'public', 'app.js'), 'utf8');
  assert.match(app, /router:switching'[\s\S]{0,200}clearStreamHealthWarnings\(\)/);
});

test('real Socket.IO selection lifecycle waits for a whitelist and never reopens a stale interface', async (t) => {
  const server = http.createServer();
  const io = new Server(server);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  const ros = streamRos();
  const roomIo = {
    get engine() { return io.engine; },
    on: io.on.bind(io),
    emit(event, data) { io.to('router-a').emit(event, data); },
    to(room) { return io.to('router-a').to(room); },
  };
  const collector = new TrafficCollector({ ros, io: roomIo, defaultIf: 'old-wan', historyMinutes: 1, state: {} });
  collector.start();
  assert.equal(ros.streams.length, 0, 'pending whitelist opens no optimistic stream');
  collector.setAvailableInterfaces([{ name: 'old-wan', running: true, disabled: false }]);
  assert.match(ros.streams.at(-1).params[0], /old-wan/);

  io.on('connection', socket => {
    socket.join('router-a');
    collector.bindSocket(socket);
    socket.emit('interfaces:list', { ok: true, defaultIf: 'old-wan', interfaces: [{ name: 'old-wan', running: true, disabled: false }] });
  });
  const client = connectClient(`http://127.0.0.1:${port}`, { transports: ['websocket'] });
  t.after(async () => {
    client.close(); collector.stop(); await io.close(); await new Promise(resolve => server.close(resolve));
  });
  await new Promise((resolve, reject) => {
    client.once('interfaces:list', resolve);
    client.once('connect_error', reject);
  });

  ros.connected = false;
  ros.emit('close');
  ros.connected = true;
  ros.emit('connected');
  const countBeforeRefresh = ros.streams.length;
  await wait(20);
  assert.equal(ros.streams.length, countBeforeRefresh, 'reconnect does not reopen old-wan before refresh');

  const listPromise = new Promise(resolve => client.once('interfaces:list', data => {
    client.emit('traffic:select', { ifName: data.interfaces[0].name });
    resolve(data);
  }));
  collector.setAvailableInterfaces([{ name: 'new-wan', running: false, disabled: false }]);
  roomIo.emit('interfaces:list', { ok: true, defaultIf: 'old-wan', interfaces: [{ name: 'new-wan', running: false, disabled: false }] });
  const refreshed = await listPromise;
  assert.equal(refreshed.interfaces[0].name, 'new-wan');
  await wait(20);
  assert.match(ros.streams.at(-1).params[0], /new-wan/);
  assert.doesNotMatch(ros.streams.at(-1).params[0], /old-wan/);
});

test('release promotion guard is monotonic and workflow is serialized', () => {
  assert.equal(mayPromote('v0.7.8-cn.4', ['v0.7.8-cn.3', 'v0.7.8-cn.4']), true);
  assert.equal(mayPromote('v0.7.8-cn.3', ['v0.7.8-cn.3', 'v0.7.8-cn.4']), false);
  assert.equal(mayPromote('v0.7.8', ['v0.7.8']), false);
  const workflow = fs.readFileSync(path.join(__dirname, '..', '.github', 'workflows', 'docker-publish.yml'), 'utf8');
  assert.match(workflow, /group: mikrodash-cn-latest-promotion/);
  assert.match(workflow, /git merge-base --is-ancestor "\$tag_commit" origin\/main/);
  assert.match(workflow, /release-guard\.js/);
  assert.match(workflow, /\.starting == true/);
});
