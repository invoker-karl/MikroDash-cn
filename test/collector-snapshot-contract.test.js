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
