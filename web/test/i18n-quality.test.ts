import test from 'node:test';
import assert from 'node:assert';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { makeDoc } from './dom-shim';
import { initVpnPage, type VpnPayload } from '../src/pages/vpn';

const ROOT = process.env.MIKRODASH_ROOT || path.join(__dirname, '..', '..');
const WEB = path.join(ROOT, 'web');

function locale() {
  const sandbox: { window: Record<string, unknown> } = { window: {} };
  vm.runInNewContext(fs.readFileSync(path.join(WEB, 'public', 'locales', 'zh-CN.js'), 'utf8'), sandbox);
  return (sandbox.window as any).MikroDashLocales['zh-CN'];
}

function translate(source: string): string {
  const active = locale();
  if (Object.prototype.hasOwnProperty.call(active.messages, source)) return active.messages[source];
  for (const [pattern, replacement] of active.patterns) {
    const match = source.match(pattern);
    if (match) return source.replace(pattern, (...args: unknown[]) =>
      typeof replacement === 'function' ? replacement(...args.slice(0, -2)) : replacement);
  }
  return source;
}

test('theme and protocol product names remain intact while contextual labels translate', () => {
  const settings = fs.readFileSync(path.join(WEB, 'src', 'ui', 'page-settings.html'), 'utf8');
  assert.match(settings, /id="themeSwatches" data-i18n-skip/);
  assert.strictEqual(translate('Pro'), 'Pro');
  assert.strictEqual(translate('OpenSent'), 'OpenSent');
  assert.strictEqual(translate('OpenConfirm'), 'OpenConfirm');
  assert.strictEqual(translate('IPsec Peers'), 'IPsec 对端');
  assert.strictEqual(translate('Regional settings'), '区域设置');
  assert.strictEqual(translate('Encryption algorithm'), '加密算法');
  assert.strictEqual(translate('Authentication algorithm'), '认证算法');
});

test('native dialog patterns translate copy without translating supplied names', () => {
  assert.strictEqual(
    translate('Remove the queue "Dashboard"?\n\nTraffic it was limiting will no longer be shaped.'),
    '删除队列“Dashboard”吗？它所限制的流量将不再整形。',
  );
  assert.strictEqual(
    translate('Delete user "Admin"? This cannot be undone.'),
    '要删除用户“Admin”吗？此操作无法撤销。',
  );
  assert.strictEqual(
    translate('Release the DHCP lease on "Bridge"?\n\nThe uplink goes down until the client rebinds — usually seconds, but it is a real outage.'),
    '要释放接口“Bridge”的 DHCP 租约吗？\n\n在客户端重新绑定前，出口会中断——通常只需数秒，但这是真实的网络中断。',
  );
});

test('VPN renderers mark RouterOS values as user data', () => {
  const ids = [
    'vpnPageCount', 'vpnStatTotal', 'vpnStatConn', 'vpnStatStale', 'vpnStatIdle',
    'vpnStatThroughput', 'vpnPppCard', 'vpnPppTbody', 'vpnPppCount', 'vpnIpsecCard',
    'vpnIpsecTbody', 'vpnIpsecCount', 'vpnPageGrid',
  ];
  const doc = makeDoc(ids, {});
  const handlers: Record<string, (payload: VpnPayload) => void> = {};
  const previousDocument = globalThis.document;
  const previousWindow = globalThis.window;
  (globalThis as any).document = doc;
  (globalThis as any).window = {};
  try {
    initVpnPage({ on: (event: string, handler: (payload: VpnPayload) => void) => {
      handlers[event] = handler;
    }, emit() {} } as any, () => true);
    assert.ok(handlers['vpn:update']);
    handlers['vpn:update']!({
      ts: 1,
      pollMs: 1000,
      ppp: [{ type: 'PPP', name: 'Admin', service: 'Dashboard', address: '10.0.0.2',
        callerId: 'Bridge', uptime: '1m', rx: 1, tx: 2 }],
      ipsec: [{ type: 'IPsec', name: 'Viewer', state: 'Unknown', side: 'initiator',
        uptime: '2m', enc: 'None', auth: 'RouterOS' }],
      tunnels: [{ id: '1', publicKey: 'key', type: 'WireGuard', name: 'Dashboard',
        state: 'active', lastHandshake: '1m', keepalive: '25s', endpoint: 'RouterOS',
        allowedIp: '10.0.0.0/24', interface: 'Bridge', rx: 1, tx: 2, rxRate: 0, txRate: 0 }],
    });
    assert.match(doc.nodes.vpnPppTbody.innerHTML, /<tr data-i18n-user-data>/);
    assert.match(doc.nodes.vpnPppTbody.innerHTML, />Admin</);
    assert.match(doc.nodes.vpnIpsecTbody.innerHTML, /<tr data-i18n-user-data>/);
    assert.match(doc.nodes.vpnIpsecTbody.innerHTML, />Viewer</);
    assert.match(doc.nodes.vpnPageGrid.innerHTML, /vpn-tile-name-text" data-i18n-user-data>Dashboard</);
    assert.match(doc.nodes.vpnPageGrid.innerHTML, /vpn-tile-iface" data-i18n-user-data>Bridge/);
    assert.match(doc.nodes.vpnPageGrid.innerHTML, /vpn-tile-ip" data-i18n-user-data>RouterOS/);
  } finally {
    if (previousDocument === undefined) delete (globalThis as any).document;
    else globalThis.document = previousDocument;
    if (previousWindow === undefined) delete (globalThis as any).window;
    else globalThis.window = previousWindow;
  }
});
