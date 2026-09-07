/**
 * THE STANDARD COLUMN ON THE WIFI CLIENTS TABLE.
 *
 * ── WHAT IT IS FOR ──────────────────────────────────────────────────────────
 *
 * Band says WHICH RADIO a client is on; Standard says WHICH GENERATION it
 * negotiated with that radio. They are orthogonal, and the gap between them is
 * the point: a client reading 5GHz + Wi-Fi 5 on a Wi-Fi 6 access point is not
 * getting Wi-Fi 6, and nothing else on the page would say so. Measured on a live
 * hAP AX3 on 2026-09-07, where a client was doing exactly that (`band=5ghz-ac`).
 *
 * ── DRIVEN THROUGH THE PAGE, NOT THROUGH THE BADGE ──────────────────────────
 *
 * `standardBadge` could be perfectly correct and simply not wired into the row,
 * which is a failure this port has shipped before — "written but never called".
 * So this boots the real page module and feeds it a real `wireless:update`,
 * which exercises the collector's field name, the row builder and the badge in
 * one go. A unit test on the badge alone would pass against a table that never
 * renders it.
 */

import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert';
import { execFileSync } from 'node:child_process';
import { makeDoc } from './dom-shim.js';

const say = console.log.bind(console);
const ROOT = process.env.MIKRODASH_ROOT || path.join(__dirname, '..', '..');

const ENTRY = path.join(ROOT, 'testdata', '.wl-entry.ts');
fs.writeFileSync(ENTRY, "export { initWirelessPage } from '../web/src/pages/wireless.js';\n");
const OUT = path.join(ROOT, 'testdata', '.wl.cjs');
execFileSync(path.join(ROOT, 'web', 'node_modules', '.bin', 'esbuild'),
  [ENTRY, '--bundle', '--format=cjs', '--platform=node', '--outfile=' + OUT, '--log-level=warning'],
  { stdio: 'inherit' });
fs.rmSync(ENTRY, { force: true });

const IDS = [
  'wirelessTable', 'wlThead', 'wlSsidList', 'wlBandGrid',
  'wlSigBarE', 'wlSigBarG', 'wlSigBarF', 'wlSigBarP',
  'wlSigCntE', 'wlSigCntG', 'wlSigCntF', 'wlSigCntP',
  'wlBandNum24', 'wlBandNum5', 'wlBandNum6', 'wlBandRow6',
  'wlSigNoData', 'wlCount', 'wlSearch', 'wlFaBtn',
];

function client(over) {
  return Object.assign({
    mac: '02:00:00:00:00:01', signal: -50, iface: '5GHz WiFi', txRate: '866Mbps',
    band: '5GHz', standard: 'Wi-Fi 5', ip: '', rxRate: '', uptime: '1h',
    ssid: 'Net', name: 'device',
  }, over);
}

function boot() {
  const doc = makeDoc(IDS);
  const handlers = {};
  const socket = { on: (ev, fn) => { handlers[ev] = fn; }, emit: () => {} };
  const prevDoc = global.document;
  const prevWin = global.window;
  global.document = doc;
  global.window = { addEventListener: () => {}, setTimeout, clearTimeout };
  const { initWirelessPage } = require(OUT);
  initWirelessPage(socket, () => true);
  return {
    doc,
    send: (clients) => handlers['wireless:update']({
      ts: 1, pollMs: 30000, mode: 'wifi', capsmanAvailable: false,
      clients, ssids: [], ssidsManagedElsewhere: 0,
    }),
    restore: () => { global.document = prevDoc; global.window = prevWin; },
  };
}

// ── 1. every generation renders in its own colour ───────────────────────────
{
  const { doc, send, restore } = boot();
  send([
    client({ mac: '02:00:00:00:00:01', standard: 'Wi-Fi 4' }),
    client({ mac: '02:00:00:00:00:02', standard: 'Wi-Fi 5' }),
    client({ mac: '02:00:00:00:00:03', standard: 'Wi-Fi 6' }),
    client({ mac: '02:00:00:00:00:04', standard: 'Wi-Fi 6E' }),
    client({ mac: '02:00:00:00:00:05', standard: 'Wi-Fi 7' }),
    client({ mac: '02:00:00:00:00:06', standard: 'Legacy' }),
  ]);
  const html = String(doc.nodes['wirelessTable'].innerHTML);
  restore();

  // THE CLASS IS THE COLOUR. A distinct class per generation is what was asked
  // for, so asserting the label alone would pass against six identical grey
  // pills — which is the version of this feature that is not worth having.
  const want = [
    ['Wi-Fi 4', 'wl-std-4'], ['Wi-Fi 5', 'wl-std-5'], ['Wi-Fi 6', 'wl-std-6'],
    ['Wi-Fi 6E', 'wl-std-6e'], ['Wi-Fi 7', 'wl-std-7'], ['Legacy', 'wl-std-legacy'],
  ];
  for (const [label, cls] of want) {
    assert.ok(html.includes(cls),
      'no "' + cls + '" pill rendered, so ' + label + ' has no colour of its own:\n' + html);
    assert.ok(html.includes('>' + label + '<'),
      'the ' + label + ' pill does not carry its label:\n' + html);
  }
  const classes = new Set((html.match(/wl-std-[a-z0-9]+/g) || []));
  assert.strictEqual(classes.size, 6,
    'six generations rendered ' + classes.size + ' distinct colours; two share one, ' +
    'so the column cannot be read at a glance: ' + [...classes].join(', '));
  say('ok  each standard renders its own coloured pill');
}

// ── 2. an unknown generation is a DASH, not a blank pill ────────────────────
//
// A CAPsMAN row carries no band at all — `wlBandOf` falls back to the interface
// name for the band, and there is nothing to fall back to for the generation. An
// empty pill reads as a value; the dash matches every other column's absence.
{
  const { doc, send, restore } = boot();
  send([client({ standard: '', band: '5GHz' })]);
  const html = String(doc.nodes['wirelessTable'].innerHTML);
  restore();
  assert.ok(/&mdash;|—/.test(html),
    'a client with no known standard rendered nothing at all; the cell must say ' +
    '"not known" rather than look like a rendering failure:\n' + html);
  assert.ok(!/wl-std\s/.test(html) && !/wl-std"/.test(html),
    'a client with no known standard rendered a pill rather than a dash:\n' + html);
  // AND THE BAND MUST SURVIVE IT. The two are independent, and a CAPsMAN row
  // has a band while having no generation.
  assert.ok(/wl-band/.test(html),
    'the band pill vanished for a client with no standard, so the new column ' +
    'has broken the old one:\n' + html);
  say('ok  an unknown standard is a dash, and the band pill still renders');
}

// ── 3. the column is in the header, to the RIGHT of Band ────────────────────
//
// That position was asked for specifically, and the markup comment on the static
// header says it "must match renderWireless()'s column list" — two places that
// can disagree silently.
{
  const { doc, send, restore } = boot();
  send([client({})]);
  const head = String(doc.nodes['wlThead'].innerHTML);
  restore();
  const band = head.indexOf('Band');
  const std = head.indexOf('Standard');
  assert.ok(band >= 0, 'the Band header is gone:\n' + head);
  assert.ok(std >= 0, 'no Standard header was rendered:\n' + head);
  assert.ok(std > band,
    'Standard is rendered to the LEFT of Band; it was asked for on the right ' +
    'because it qualifies the band:\n' + head);
  say('ok  Standard sits to the right of Band in the header');
}

// ── 4. the row still has as many cells as the header has columns ────────────
//
// Adding a column means several edits — colgroup, thead, the row, and two
// colspans — and a missed one shifts every later cell under the wrong heading
// without throwing anything.
{
  const { doc, send, restore } = boot();
  send([client({})]);
  const head = String(doc.nodes['wlThead'].innerHTML);
  const body = String(doc.nodes['wirelessTable'].innerHTML);
  restore();
  const ths = (head.match(/<th\b/g) || []).length;
  const tds = (body.match(/<td\b/g) || []).length;
  assert.strictEqual(tds, ths,
    'the row renders ' + tds + ' cells against ' + ths + ' headers, so every ' +
    'column after the mismatch sits under the wrong heading.\n' + head + '\n' + body);
  say('ok  the row has one cell per header column (' + ths + ')');
}

fs.rmSync(OUT, { force: true });
say('wifi-standard-column: all checks passed');
