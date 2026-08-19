'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const EventEmitter = require('node:events');
const DhcpNetworksCollector = require('../src/collectors/dhcpNetworks');
const ConnectionsCollector = require('../src/collectors/connections');
const FirewallCollector = require('../src/collectors/firewall');
const RoutingCollector = require('../src/collectors/routing');
const VpnCollector = require('../src/collectors/vpn');
const DhcpLeasesCollector = require('../src/collectors/dhcpLeases');
const ArpCollector = require('../src/collectors/arp');
const InterfaceStatusCollector = require('../src/collectors/interfaceStatus');
const TopTalkersCollector = require('../src/collectors/talkers');
const WirelessCollector = require('../src/collectors/wireless');

function flush() { return new Promise(resolve => setImmediate(resolve)); }

function ioStub(clients = 1) {
  const events = [];
  const target = {
    events, engine: { clientsCount: clients }, on() {},
    emit(event, data) { events.push({ event, data }); },
    to() { return target; },
    sockets: { adapter: { rooms: new Map() } },
  };
  return target;
}

function rosStub(write) {
  const ros = new EventEmitter();
  ros.connected = true;
  ros.write = write || (async () => []);
  ros.streams = [];
  ros.stream = (words, ...args) => {
    const callback = args.find(arg => typeof arg === 'function') || null;
    const stream = new EventEmitter();
    stream.stop = () => {};
    stream.callback = callback;
    stream.words = words;
    stream.args = args;
    ros.streams.push(stream);
    return stream;
  };
  return ros;
}

test('DHCP Networks preserves rejected tables and accepts successful empty snapshots', async () => {
  let failNetworks = true;
  const ros = rosStub(async cmd => {
    if (cmd.includes('network')) {
      if (failNetworks) throw new Error('temporary timeout');
      return [];
    }
    return [];
  });
  const state = {};
  const c = new DhcpNetworksCollector({
    ros, io: ioStub(), pollMs: 5000, state, wanIface: 'wan',
    dhcpLeases: { getAllLeaseIPs: () => [] },
  });
  c._raw.networks = [{ address: '192.168.1.0/24' }];
  await c._fetchOnce();
  assert.equal(c._raw.networks.length, 1, 'temporary failure keeps last good network table');
  assert.match(state.lastNetworksErr, /temporary timeout/);
  failNetworks = false;
  await c._fetchOnce();
  assert.deepEqual(c._raw.networks, [], 'successful ordinary empty result clears the table');
});

test('Connections authoritative empty clears cache while a failed idle confirmation preserves it', async () => {
  let reject = true;
  const ros = rosStub(async () => {
    if (reject) throw new Error('temporary timeout');
    return [];
  });
  const deposits = [];
  const c = new ConnectionsCollector({
    ros, io: ioStub(), pollMs: 5000, topN: 5, maxConns: 100,
    dhcpNetworks: { getLanCidrs: () => [] },
    dhcpLeases: { getNameByIP: () => null, getNameByMAC: () => null },
    arp: { getByIP: () => null }, state: {}, geoLookup: null,
    connTableCache: { deposit: rows => deposits.push(rows) },
  });
  c._rowsPrev = [{ '.id': '*1' }];
  c._snapshotProbe.onIdle();
  await flush();
  assert.equal(c._rowsPrev.length, 1);
  c._snapshotProbe.invalidate();
  reject = false;
  c._snapshotProbe.onIdle();
  await flush();
  assert.deepEqual(c._rowsPrev, []);
  assert.deepEqual(deposits.at(-1), []);
});

test('Connections authoritative non-empty snapshot atomically accepts a large real contraction', () => {
  const deposits = [];
  const c = new ConnectionsCollector({
    ros: rosStub(), io: ioStub(), pollMs: 5000, topN: 5, maxConns: 1000,
    dhcpNetworks: { getLanCidrs: () => [] },
    dhcpLeases: { getNameByIP: () => null, getNameByMAC: () => null },
    arp: { getByIP: () => null }, state: {}, geoLookup: null,
    connTableCache: { deposit: rows => deposits.push(rows) },
  });
  c._rowsPrev = Array.from({ length: 100 }, (_, i) => ({ '.id': `*${i}` }));
  c._rowsNext = Array.from({ length: 10 }, (_, i) => ({ '.id': `*new${i}` }));
  c._onBatchComplete(true);
  assert.equal(c._rowsPrev.length, 10);
  assert.equal(deposits.at(-1).length, 10);
  assert.equal(c._partialStreak, 0);
});

test('Connections rejected partial burst does not advance the shared rate snapshot', () => {
  const deposits = [];
  const c = new ConnectionsCollector({
    ros: rosStub(), io: ioStub(0), pollMs: 5000, topN: 5, maxConns: 1000,
    dhcpNetworks: { getLanCidrs: () => [] },
    dhcpLeases: { getNameByIP: () => null, getNameByMAC: () => null },
    arp: { getByIP: () => null }, state: {}, geoLookup: null,
    connTableCache: { deposit: rows => deposits.push(rows) },
  });
  c._rowsPrev = Array.from({ length: 100 }, (_, i) => ({ '.id': `*old${i}` }));
  c._rowsNext = Array.from({ length: 5 }, (_, i) => ({ '.id': `*partial${i}` }));
  c._onBatchComplete(false);
  assert.equal(deposits.length, 0, 'retained old counters cannot be timestamped as a fresh rate sample');
  assert.equal(c._rowsPrev.length, 100);
});

test('Firewall failed confirmation preserves rules and successful empty removes them', async () => {
  let reject = true;
  const ros = rosStub(async () => {
    if (reject) throw new Error('API busy');
    return [];
  });
  const state = {};
  const c = new FirewallCollector({ ros, io: ioStub(), pollMs: 10000, state });
  c._filter = [{ id: '*1', chain: 'input', packets: 1, bytes: 2 }];
  c._snapshotProbe.onIdle();
  await flush();
  assert.equal(c._filter.length, 1);
  assert.match(state.lastFirewallErr, /API busy/);
  c._snapshotProbe.invalidate();
  reject = false;
  c._snapshotProbe.onIdle();
  await flush();
  assert.deepEqual(c._filter, []);
});

test('Routing listen synthetic arrays neither mutate routes nor refresh health', () => {
  const ros = rosStub();
  const state = { lastRoutingTs: 123 };
  const c = new RoutingCollector({ ros, io: ioStub(), pollMs: 30000, state });
  c._routes.set('*1', { _id: '*1', _raw: {}, flags: {}, type: 'static' });
  c._startRouteStream();
  ros.streams.at(-1).callback(null, []);
  assert.equal(c._routes.size, 1);
  assert.equal(state.lastRoutingTs, 123);
});

test('VPN structural listen ignores synthetic arrays and transient PPP/IPsec failures preserve sessions', async () => {
  let transient = false;
  const ros = rosStub(async cmd => {
    if (transient && (cmd.includes('/ppp/') || cmd.includes('/ip/ipsec/'))) throw new Error('temporary timeout');
    if (cmd.includes('/ppp/')) return [{ name: 'office', service: 'l2tp' }];
    if (cmd.includes('active-peers')) return [{ 'remote-address': '203.0.113.1' }];
    if (cmd.includes('installed-sa')) return [];
    return [];
  });
  const c = new VpnCollector({ ros, io: ioStub(), pollMs: 10000, state: {} });
  await c._loadOtherVpns();
  assert.equal(c._ppp.length, 1);
  assert.equal(c._ipsec.length, 1);
  transient = true;
  await c._loadOtherVpns();
  assert.equal(c._ppp.length, 1);
  assert.equal(c._ipsec.length, 1);
  c._startStream();
  ros.streams.at(-1).callback(null, []);
  assert.equal(c._peers.has('?'), false, 'synthetic listen idle cannot create a ghost peer');
});

test('DHCP lease and ARP snapshots atomically clear on success and preserve on failure', async () => {
  let fail = false;
  let empty = false;
  const ros = rosStub(async cmd => {
    if (fail && (cmd.includes('/lease/print') || cmd.includes('/arp/print'))) throw new Error('temporary timeout');
    if (cmd.includes('/lease/print')) return empty ? [] : [{ address: '192.0.2.2', 'mac-address': 'AA', status: 'bound' }];
    if (cmd.includes('/arp/print')) return empty ? [] : [{ address: '192.0.2.3', 'mac-address': 'BB' }];
    return [];
  });
  const leases = new DhcpLeasesCollector({ ros, io: ioStub(), state: {}, pollMs: 5000 });
  const arp = new ArpCollector({ ros, state: {}, pollMs: 5000 });
  await leases._loadInitial();
  await arp._loadInitial();
  assert.equal(leases.byIP.size, 1);
  assert.equal(arp.byIP.size, 1);
  fail = true;
  await leases._loadInitial();
  await arp._loadInitial();
  assert.equal(leases.byIP.size, 1);
  assert.equal(arp.byIP.size, 1);
  fail = false;
  empty = true;
  await leases._loadInitial();
  await arp._loadInitial();
  assert.equal(leases.byIP.size, 0);
  assert.equal(arp.byIP.size, 0);
});

test('InterfaceStatus authoritative empty metadata removes the final address and interface', () => {
  const metadata = [];
  const ros = rosStub();
  const c = new InterfaceStatusCollector({
    ros, io: ioStub(), pollMs: 5000, metaPollMs: 60000, state: {},
    onInterfaceMetadata: rows => metadata.push(rows),
  });
  c._ifaces.set('wan', { name: 'wan', running: true, disabled: false });
  c._addrs.set('wan', ['192.0.2.1/24']);
  c._applyAuthoritativeMeta('addresses', []);
  assert.equal(c._addrs.size, 0);
  c._applyAuthoritativeMeta('interfaces', []);
  assert.equal(c._ifaces.size, 0);
  assert.deepEqual(metadata.at(-1), []);
});

test('Talkers coalesces idle probes and stop invalidates a late authoritative empty', async () => {
  let resolveRead;
  let reads = 0;
  const ros = rosStub(() => {
    reads++;
    return new Promise(resolve => { resolveRead = resolve; });
  });
  const c = new TopTalkersCollector({
    ros, io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: true,
  });
  c._replaceAuthoritativeRows([{ name: 'phone', 'mac-address': 'AA', 'rate-up': '1', 'rate-down': '2' }]);
  c.start();
  const stream = ros.streams.at(-1);
  stream.emit('data', []);
  stream.emit('data', []);
  await flush();
  assert.equal(reads, 1, 'synthetic idle bursts share one ordinary print');
  c.stop();
  resolveRead([]);
  await flush();
  assert.equal(c.lastPayload.devices.length, 1, 'late pre-stop empty cannot clear the last good rows');
});

test('Talkers temporary confirmation failure preserves data and permission is distinct from empty', async () => {
  let error = new Error('temporary timeout');
  const ros = rosStub(async () => { throw error; });
  const c = new TopTalkersCollector({
    ros, io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: true,
  });
  c._replaceAuthoritativeRows([{ name: 'phone', 'mac-address': 'AA', 'rate-up': '1', 'rate-down': '2' }]);
  c._snapshotProbe.onIdle();
  await flush();
  assert.equal(c.lastPayload.devices.length, 1);
  assert.match(c.state.lastTalkersErr, /temporary timeout/);
  c._snapshotProbe.invalidate();
  error = new Error('permission denied');
  c._snapshotProbe.onIdle();
  await flush();
  assert.equal(c.lastPayload.unavailable, true);
  assert.equal(c.lastPayload.reason, 'Kid Control permission denied');
  c.stop();
});

test('Talkers uses the stats view for stream, idle confirmation, and poll snapshots', async () => {
  const calls = [];
  const row = { '.id': '*1', name: 'phone', 'mac-address': 'AA', 'rate-up': '2Mbps', 'rate-down': '14.7Mbps' };
  let statsRows = [row];
  const ros = rosStub(async (cmd, args) => {
    calls.push({ cmd, args });
    return args.includes('=stats=') ? statsRows : [];
  });
  const c = new TopTalkersCollector({
    ros, io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: true,
  });
  c.start();
  const stream = ros.streams.at(-1);
  assert.ok(stream.args[0].includes('=stats='));
  assert.equal(stream.args[0].some(word => word.startsWith('=.proplist=')), false);

  stream.emit('data', row);
  clearTimeout(c._commitTimer);
  c._commitTick();
  assert.equal(c.lastPayload.devices[0].rx_mbps, 14.7);
  stream.emit('data', []);
  await flush();
  assert.equal(c.lastPayload.devices.length, 1,
    'a synthetic idle is confirmed with stats, not cleared by a plain-print false empty');
  assert.deepEqual(calls.at(-1).args, ['=stats=']);

  c._snapshotProbe.invalidate();
  statsRows = [];
  c._snapshotProbe.onIdle();
  await flush();
  assert.deepEqual(c.lastPayload.devices, [], 'a genuinely empty stats snapshot clears the card');
  const emptyEmits = c.io.events.filter(event => event.event === 'talkers:update' && event.data.devices.length === 0).length;
  c._snapshotProbe.invalidate();
  c._snapshotProbe.onIdle();
  await flush();
  assert.equal(c.io.events.filter(event => event.event === 'talkers:update' && event.data.devices.length === 0).length,
    emptyEmits, 'repeated authoritative empty does not emit duplicate clears');

  c.streamMode = false;
  statsRows = [row];
  await c._pollTalkersOnce();
  assert.deepEqual(calls.at(-1).args, ['=stats=']);
  c.stop();
});

test('Talkers falls back to byte-counter deltas and accepts a stable .id without MAC', () => {
  const c = new TopTalkersCollector({
    ros: rosStub(), io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: false,
  });
  const first = c._normaliseDevice({ '.id': '*7', name: 'wired', 'bytes-up': '1MiB', 'bytes-down': '2MiB' }, 1000);
  const second = c._normaliseDevice({ '.id': '*7', name: 'wired', 'bytes-up': '2MiB', 'bytes-down': '4MiB' }, 2000);
  const direct = c._normaliseDevice({ '.id': '*8', name: 'wifi', 'rate-up': 2500000, 'rate-down': '14.7Mbps' }, 2000);
  assert.equal(first.key, '*7');
  assert.equal(second.rateUp, 8 * 1024 * 1024);
  assert.equal(second.rateDown, 16 * 1024 * 1024);
  assert.equal(direct.rateUp, 2500000);
  assert.equal(direct.rateDown, 14700000);
  assert.equal(c._normaliseDevice({ name: 'duplicate-name-only', 'rate-up': 1 }, 3000), null,
    'a mutable or duplicate name is not a stable identity');
  c.stop();
});

test('Talkers prefers connection-derived LAN rates, including an authoritative empty list', () => {
  const ros = rosStub();
  const io = ioStub();
  const c = new TopTalkersCollector({
    ros, io, pollMs: 3000, state: {}, topN: 2, streamMode: true,
    connectionStaleMs: 60000,
  });
  c.start();
  assert.ok(c._stream, 'Kid Control starts only as the compatibility fallback');

  c._replaceAuthoritativeRows([
    { '.id': '*kid', name: 'old-kid-row', 'rate-up': '1Mbps', 'rate-down': '1Mbps' },
  ]);
  c.acceptConnectionPayload({
    ts: 1234, pollMs: 5000,
    devices: [
      { srcIp: '192.168.1.20', name: 'laptop', mac: 'AA:BB', rxMbps: 3, txMbps: 4 },
      { srcIp: '192.168.1.30', name: '', mac: '', rxMbps: 9, txMbps: 2 },
      { srcIp: '192.168.1.40', name: 'third', mac: 'CC:DD', rxMbps: 1, txMbps: 1 },
    ],
  });

  assert.equal(c._stream, null, 'preferred source closes the redundant Kid Control stream');
  assert.equal(c.lastPayload.source, 'connections');
  assert.equal(c.lastPayload.devices.length, 2, 'the configured dashboard top-N is enforced');
  assert.equal(c.lastPayload.devices[0].name, '192.168.1.30', 'an IP is the safe name fallback');
  assert.equal(c.lastPayload.devices[1].name, 'laptop');
  assert.equal(JSON.stringify(c.lastPayload).includes('srcIp'), false,
    'internal connection identity is not exposed in the compact dashboard payload');

  c.acceptConnectionPayload({ ts: 2345, pollMs: 5000, devices: [] });
  assert.deepEqual(c.lastPayload.devices, []);
  assert.equal(c.lastPayload.source, 'connections');
  assert.equal(c.lastPayload.emptyText, 'No active LAN devices');
  c.stop();
});

test('Talkers merges multiple addresses by normalized MAC and never exposes RouterOS internal ids', () => {
  const c = new TopTalkersCollector({
    ros: rosStub(), io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: false,
    connectionStaleMs: 60000,
  });
  c.start();
  c.acceptConnectionPayload({
    ts: 1, pollMs: 5000,
    devices: [
      { srcIp: '192.168.1.2', name: 'phone', mac: 'aa-bb-cc-dd-ee-ff', rxMbps: 1, txMbps: 2 },
      { srcIp: '2001:db8::2', name: 'phone', mac: 'AA:BB:CC:DD:EE:FF', rxMbps: 3, txMbps: 4 },
    ],
  });
  assert.equal(c.lastPayload.devices.length, 1);
  assert.equal(c.lastPayload.devices[0].mac, 'AA:BB:CC:DD:EE:FF');
  assert.equal(c.lastPayload.devices[0].rx_mbps, 4);
  assert.equal(c.lastPayload.devices[0].tx_mbps, 6);
  assert.equal(JSON.stringify(c.lastPayload).includes('192.168.1.2'), false);

  c._clearConnectionPayload(false);
  c._replaceAuthoritativeRows([{ '.id': '*secret', 'rate-up': 1, 'rate-down': 2 }]);
  assert.equal(c.lastPayload.devices[0].name, 'Unknown device');
  assert.equal(JSON.stringify(c.lastPayload).includes('*secret'), false);
  c.stop();
});

test('Talkers does not let Kid Control race or stale data override a fresh connection source', () => {
  const ros = rosStub();
  const c = new TopTalkersCollector({
    ros, io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: true,
    connectionStaleMs: 60000,
  });
  c.start();
  c.acceptConnectionPayload({
    ts: 1000, pollMs: 5000,
    devices: [{ srcIp: '10.0.0.2', name: 'connection-device', rxMbps: 5, txMbps: 1 }],
  });
  c._replaceAuthoritativeRows([
    { '.id': '*late', name: 'late-kid-row', 'rate-up': '99Mbps', 'rate-down': '99Mbps' },
  ]);
  assert.equal(c.lastPayload.source, 'connections');
  assert.equal(c.lastPayload.devices[0].name, 'connection-device');

  c.acceptConnectionPayload(null);
  assert.equal(c.lastPayload.unavailable, true, 'source loss never replays the stale Kid Control row');
  assert.equal(c.lastPayload.reason, 'Device traffic is unavailable');
  assert.ok(c._stream, 'Kid Control restarts only after the preferred source is lost');
  c.stop();
});

test('Talkers instances keep connection-derived devices isolated per router session', () => {
  const a = new TopTalkersCollector({
    ros: rosStub(), io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: false,
    connectionStaleMs: 60000,
  });
  const b = new TopTalkersCollector({
    ros: rosStub(), io: ioStub(), pollMs: 3000, state: {}, topN: 5, streamMode: false,
    connectionStaleMs: 60000,
  });
  a.start();
  b.start();
  a.acceptConnectionPayload({
    ts: 1, pollMs: 5000,
    devices: [{ srcIp: '10.0.0.2', name: 'router-a', rxMbps: 1, txMbps: 2 }],
  });
  b.acceptConnectionPayload({
    ts: 2, pollMs: 5000,
    devices: [{ srcIp: '192.168.1.2', name: 'router-b', rxMbps: 3, txMbps: 4 }],
  });
  assert.equal(a.lastPayload.devices[0].name, 'router-a');
  assert.equal(b.lastPayload.devices[0].name, 'router-b');
  a.stop();
  b.stop();
});

test('Wireless synthetic idle and transient confirmation do not age clients or switch mode', async () => {
  let fail = true;
  const ros = rosStub(async () => {
    if (fail) throw new Error('temporary timeout');
    return [];
  });
  const c = new WirelessCollector({
    ros, io: ioStub(), pollMs: 5000, state: {}, streamMode: true,
    dhcpLeases: { getNameByMAC: () => null }, arp: { getByMAC: () => null },
  });
  const row = { 'mac-address': 'AA', interface: 'wifi1', signal: '-50' };
  c._onBatch('wifi', [row]);
  c._startStream('wifi');
  ros.streams.at(-1).emit('data', []);
  await flush();
  assert.equal(c.mode, 'wifi');
  assert.equal(c._knownClients.size, 1);
  assert.equal(c._absentTicks.get('AA') || 0, 0);
  c._snapshotProbes.wifi.invalidate();
  fail = false;
  c._snapshotProbes.wifi.onIdle();
  await flush();
  assert.equal(c._knownClients.size, 1, 'one authoritative absence uses the normal absence threshold');
  assert.equal(c._absentTicks.get('AA'), 1);
  c.stop();
});

test('Wireless first authoritative empty selects wifi and two unsupported stacks do not bounce', () => {
  const ros = rosStub();
  const c = new WirelessCollector({
    ros, io: ioStub(), pollMs: 5000, state: {}, streamMode: true,
    dhcpLeases: { getNameByMAC: () => null }, arp: { getByMAC: () => null },
  });
  c._onBatch('wifi', []);
  assert.equal(c.mode, 'wifi');
  c._handleSnapshotError('wifi', new Error('no such command'));
  assert.equal(c.mode, 'wireless');
  c._handleSnapshotError('wireless', new Error('unknown command'));
  assert.equal(c.mode, null);
  assert.equal(c._streams.wifi, null);
  assert.equal(c._streams.wireless, null);
  c.stop();
});

test('VPN malformed snapshot and structural objects cannot create or delete question-mark peers', () => {
  const ros = rosStub();
  const c = new VpnCollector({ ros, io: ioStub(), pollMs: 10000, state: {} });
  c._replacePeers([{}, { 'public-key': 'stable', name: 'peer' }]);
  assert.deepEqual([...c._peers.keys()], ['stable']);
  c._startStream();
  const stream = ros.streams.at(-1);
  stream.callback(null, {});
  stream.callback(null, { '.dead': 'true' });
  assert.deepEqual([...c._peers.keys()], ['stable']);
  stream.callback(null, { '.dead': 'true', 'public-key': 'stable' });
  assert.equal(c._peers.size, 0);
  c.stop();
});

test('Firewall counter structure change confirms a full snapshot and removes deleted rules', async () => {
  const ros = rosStub(async () => [{ '.id': '*1', chain: 'input', packets: '3', bytes: '4' }]);
  const c = new FirewallCollector({ ros, io: ioStub(), pollMs: 10000, state: {} });
  c._snapshotProbe.cooldownMs = 0;
  c._filter = [
    { id: '*1', chain: 'input', packets: 1, bytes: 1 },
    { id: '*2', chain: 'input', packets: 1, bytes: 1 },
  ];
  c._staging = [{ '.id': '*1', packets: '3', bytes: '4' }];
  c._scheduleSnapshotFlush();
  await new Promise(resolve => setTimeout(resolve, 180));
  await flush();
  assert.deepEqual(c._filter.map(row => row.id), ['*1']);
  c.stop();
});
