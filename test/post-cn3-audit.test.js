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
const InterfaceStatusCollector = require('../src/collectors/interfaceStatus');
const TopTalkersCollector = require('../src/collectors/talkers');
const DhcpNetworksCollector = require('../src/collectors/dhcpNetworks');
const { computeHealthStatus } = require('../src/health');
const { applySessionInterfaceMetadata } = require('../src/interfaceMetadata');
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
    stream.stopCalls = 0;
    stream.stop = () => { stream.stopCalls += 1; };
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
  const stream = collector._stream;
  stream.emit('error', new Error('unknown command'));
  assert.equal(stream.stopCalls, 1, 'errored RouterOS stream is stopped before its reference is cleared');
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
  const start = app.indexOf('function _setInterfacesPending');
  const end = app.indexOf("ifaceSelect.addEventListener('change'", start);
  const source = app.slice(start, end);
  const namesStart = app.indexOf("socket.on('ifstatus:names'");
  const namesSource = app.slice(namesStart, app.indexOf("socket.on('ifstatus:update'", namesStart));
  const dom = new JSDOM('<select id="iface"></select>');
  const handlers = {};
  const emitted = [];
  const context = {
    document: dom.window.document,
    ifaceSelect: dom.window.document.getElementById('iface'),
    currentIf: 'wan', _ifaceSelectKey: '', _serverDefaultIf: '', _interfacesReady: false,
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

  // The router-wide heartbeat follows interfaces:list in real life. It must
  // not regress the selector to active-only strings or emit a new fallback.
  vm.runInNewContext(namesSource, context);
  const afterList = emitted.length;
  handlers['ifstatus:names']({
    interfaces: [{ name: 'wan', running: false, disabled: false }, { name: 'lan', running: true, disabled: false }],
  });
  assert.deepEqual(Array.from(context.ifaceSelect.options, option => option.value), ['wan', 'lan']);
  assert.match(context.ifaceSelect.options[0].textContent, /down/);
  assert.equal(emitted.length, afterList, 'heartbeat does not create a spurious fallback');

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

test('router switching ignores queued old interface names until the new authoritative list', () => {
  const app = fs.readFileSync(path.join(__dirname, '..', 'public', 'app.js'), 'utf8');
  const selectorStart = app.indexOf('function _setInterfacesPending');
  const selectorSource = app.slice(selectorStart, app.indexOf("ifaceSelect.addEventListener('change'", selectorStart));
  const namesStart = app.indexOf("socket.on('ifstatus:names'");
  const namesSource = app.slice(namesStart, app.indexOf("socket.on('ifstatus:update'", namesStart));
  const switchingStart = app.indexOf("socket.on('router:switching', function () {");
  const switchingSource = app.slice(switchingStart, app.indexOf('\n});', switchingStart) + 4);
  const dom = new JSDOM('<select id="iface"></select>');
  const handlers = {};
  const emitted = [];
  const context = {
    ifaceSelect: dom.window.document.getElementById('iface'),
    document: dom.window.document,
    currentIf: '', _ifaceSelectKey: '', _serverDefaultIf: '', _interfacesReady: false,
    clearDashboardData() {}, clearStreamHealthWarnings() {},
    socket: {
      on(name, handler) { handlers[name] = handler; },
      emit(name, payload) { emitted.push({ name, payload }); },
    },
    console: { warn() {} },
  };
  vm.runInNewContext(selectorSource, context);
  vm.runInNewContext(namesSource, context);
  vm.runInNewContext(switchingSource, context);

  handlers['interfaces:list']({
    ok: true, defaultIf: 'a-wan',
    interfaces: [{ name: 'a-wan', running: true, disabled: false }],
  });
  assert.equal(context.ifaceSelect.value, 'a-wan');

  handlers['router:switching']();
  assert.equal(context._interfacesReady, false);
  assert.equal(context._serverDefaultIf, '');
  assert.equal(context.ifaceSelect.options.length, 0);
  assert.equal(context.ifaceSelect.disabled, true);
  const afterSwitch = emitted.length;

  handlers['ifstatus:names']({
    interfaces: [{ name: 'a-wan', running: true, disabled: false }],
  });
  assert.equal(context.ifaceSelect.options.length, 0, 'queued router A names remain hidden');
  assert.equal(emitted.length, afterSwitch, 'queued router A names cannot subscribe');

  handlers['interfaces:list']({
    ok: true, defaultIf: 'b-wan',
    interfaces: [{ name: 'b-wan', running: false, disabled: false }],
  });
  assert.equal(context._interfacesReady, true);
  assert.equal(context.ifaceSelect.disabled, false);
  assert.deepEqual(Array.from(context.ifaceSelect.options, option => option.value), ['b-wan']);
  assert.equal(context.ifaceSelect.value, 'b-wan');
  assert.equal(emitted.length, afterSwitch, 'authoritative default does not need fallback subscription');
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

  const withoutTraffic = computeHealthStatus({
    startupReady: true, rosConnected: true,
    state: { trafficConfigValid: null, lastSystemTs: now }, now,
    requiredCollectors: ['system'],
  });
  assert.equal(withoutTraffic.statusCode, 200);
  assert.deepEqual(withoutTraffic.stale, []);
});

test('authoritative interface metadata prewarms selected and default status', () => {
  const ros = streamRos();
  const events = [];
  const collector = new TrafficCollector({
    ros, io: collectorIo(events), defaultIf: 'wan', historyMinutes: 1, state: {},
  });
  collector.setAvailableInterfaces([
    { name: 'wan', running: false, disabled: false },
    { name: 'lan', running: false, disabled: false },
  ]);
  assert.equal(collector.getInterfaceStatus('lan').running, false);
  assert.equal(collector.getInterfaceStatus('lan').unavailable, false);
  assert.equal(collector.lastWanStatus.ifName, 'wan');
  assert.equal(collector.lastWanStatus.running, false);
  assert.equal(events.filter(event => event.ev === 'wan:status').at(-1).data.running, false);

  const socket = new EventEmitter();
  socket.id = 'browser-1';
  const statuses = [];
  socket.on('traffic:status', status => statuses.push(status));
  collector.bindSocket(socket);
  socket.emit('traffic:select', { ifName: 'lan' });
  assert.equal(statuses.at(-1).ifName, 'lan');
  assert.equal(statuses.at(-1).running, false);
  assert.equal(statuses.at(-1).unavailable, false, 'known down is not a pending status');

  collector.setAvailableInterfaces([{ name: 'wan', running: true, disabled: false }]);
  assert.equal(collector.lastWanStatus.running, true, 'default recovery replays authoritative state');
  collector.stop();
});

test('runtime InterfaceStatus metadata reconciles one router traffic whitelist without reconnect', () => {
  const ros = streamRos();
  const trafficEvents = [];
  const traffic = new TrafficCollector({
    ros, io: collectorIo(trafficEvents), defaultIf: 'old-wan', historyMinutes: 1, state: {},
  });
  traffic.start();
  const lists = [];
  const session = { DEFAULT_IF: 'old-wan', traffic, cachedInterfaces: null, _interfacesRevision: 0 };
  const otherCalls = [];
  const otherSession = {
    DEFAULT_IF: 'other-wan', cachedInterfaces: [{ name: 'other-wan' }], _interfacesRevision: 0,
    traffic: { setAvailableInterfaces(interfaces) { otherCalls.push(interfaces); } },
  };
  const routerIo = { emit(event, data) { lists.push({ event, data }); } };
  let callbackCalls = 0;
  const status = new InterfaceStatusCollector({
    ros, io: collectorIo([], 1), pollMs: 5000, state: {}, rid: 'router-a',
    onInterfaceMetadata(interfaces) {
      callbackCalls += 1;
      applySessionInterfaceMetadata(session, routerIo, interfaces);
    },
  });

  status._ifaces.set('old-wan', { name: 'old-wan', running: true, disabled: false });
  status._buildAndEmit();
  const oldStream = ros.streams.at(-1).stream;
  assert.match(ros.streams.at(-1).params[0], /old-wan/);
  status._buildAndEmit();
  assert.equal(callbackCalls, 1, 'unchanged name/running/disabled metadata is deduplicated');

  status._ifaces = new Map([['new-wan', { name: 'new-wan', running: false, disabled: false }]]);
  status._buildAndEmit();
  assert.equal(callbackCalls, 2);
  assert.equal(oldStream.stopCalls, 1, 'old stream stops when its name leaves the authoritative set');
  assert.deepEqual(lists.at(-1).data.interfaces.map(i => i.name), ['new-wan']);
  assert.equal(lists.at(-1).data.defaultIf, 'old-wan');
  assert.deepEqual(session.cachedInterfaces.map(i => i.name), ['new-wan']);
  assert.deepEqual(otherSession.cachedInterfaces.map(i => i.name), ['other-wan']);
  assert.equal(otherCalls.length, 0, 'router-a metadata cannot mutate router-b');
  assert.equal(traffic._normalizeIfName('old-wan'), null, 'old name is rejected');
  assert.equal(traffic._normalizeIfName('new-wan'), 'new-wan', 'new name is immediately selectable');

  const socket = new EventEmitter();
  socket.id = 'browser-a';
  traffic.bindSocket(socket);
  socket.emit('traffic:select', { ifName: 'new-wan' });
  assert.match(ros.streams.at(-1).params[0], /new-wan/);
  assert.doesNotMatch(ros.streams.at(-1).params[0], /old-wan/);
  traffic.stop();

  const indexSource = fs.readFileSync(path.join(__dirname, '..', 'src', 'index.js'), 'utf8');
  assert.match(indexSource, /onInterfaceMetadata:[\s\S]{0,500}applySessionInterfaceMetadata\(session, routerIo, interfaces\)/);
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
