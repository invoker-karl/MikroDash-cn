/**
 * The Network Flow card resumes when the tab comes back.
 *
 * ── AN ANIMATION WITH ONE LEGITIMATE REASON TO STOP ────────────────────────
 *
 * `dc-card-netflow` draws three SMIL `<animateMotion>` dots on `#netDiagram`.
 * Nothing in the payload moves them, so no collector, staleness rule, dormancy
 * back-off or room suspension can stop them: the only thing that can is an
 * explicit `pauseAnimations()`, and the only reason to call one is that the
 * socket or the router is down.
 *
 * ── THE RESUME THAT CONSUMED ITSELF ────────────────────────────────────────
 *
 * Every pause was unconditional and every resume was refused while the tab was
 * hidden — with nothing to retry it later. So an outage that ENDED in a
 * background tab spent its own resume: `socket.ts` reconnects on a backoff timer
 * that keeps firing while hidden, and `router:status` arrives on that socket. The
 * operator came back to a dashboard with no banners, the dots visible again and
 * standing still, over a router the app was reporting as up. That is the report
 * "sometimes the Network Flow card animation stops".
 *
 * These cases drive the real module through that sequence. Case 3 is the other
 * direction and matters as much: the tab returning must NOT resume a diagram
 * whose router is still down, or the fix would have replaced a stuck animation
 * with a lying one.
 */

import fs from 'node:fs';
import path from 'node:path';
import assert from 'node:assert';
import { execFileSync } from 'node:child_process';
import { makeDoc } from './dom-shim';

const say = console.log.bind(console);
const ROOT = process.env.MIKRODASH_ROOT || path.join(__dirname, '..', '..');

const OUT = path.join(ROOT, 'web', 'dist', '_compare', 'port-banners.cjs');
fs.mkdirSync(path.dirname(OUT), { recursive: true });
execFileSync(path.join(ROOT, 'web', 'node_modules', '.bin', 'esbuild'),
  [path.join(ROOT, 'web', 'src', 'banners.ts'),
   '--bundle', '--format=cjs', '--platform=node', '--outfile=' + OUT, '--log-level=warning'],
  { stdio: 'inherit' });

const IDS = ['netDiagram', 'rosBanner', 'rosBannerText', 'reconnectBanner', 'liveRx', 'liveTx'];

/**
 * A fresh document AND a fresh module.
 *
 * `rosDisconnected` and `socketDown` are module-level, so a cached require would
 * carry one case's outage into the next — and the case that matters most starts
 * from "everything is up".
 */
function mount() {
  const doc = makeDoc(IDS);
  const moves: string[] = [];
  doc.nodes.netDiagram.pauseAnimations = () => moves.push('pause');
  doc.nodes.netDiagram.unpauseAnimations = () => moves.push('unpause');
  doc.hidden = false;
  doc.body = doc.createElement();
  (globalThis as any).document = doc;
  delete require.cache[require.resolve(OUT)];
  const banners = require(OUT);
  // Exactly what `wireBanners` does, and the reason the last case pins that it
  // still does it.
  banners.initDiagramVisibility();
  /** What a browser does on a tab switch: flip the flag, then fire the event. */
  const setHidden = (hidden: boolean) => {
    doc.hidden = hidden;
    doc.dispatchEvent({ type: 'visibilitychange' });
  };
  return { doc, moves, banners, setHidden };
}

let failed = 0;
function check(what: string, fn: () => void) {
  try { fn(); say('  ok   ' + what); }
  catch (e: any) { failed++; say('  FAIL ' + what + '\n       ' + e.message); }
}

say('network flow: the dots stop only while the connection is down');

check('a router outage that ends while the tab is hidden resumes on return', () => {
  const { moves, banners, setHidden } = mount();
  banners.onSocketConnect();
  banners.setRosBanner(true);
  moves.length = 0;

  banners.setRosBanner(false, 'RouterOS not connected');
  assert.deepEqual(moves, ['pause'], 'the router going down did not pause the diagram');

  setHidden(true);
  banners.setRosBanner(true);           // the router came back, unseen
  setHidden(false);                     // …and the operator comes back to the tab

  assert.ok(moves.includes('unpause'),
    'the diagram was never unpaused: the router recovered while the tab was ' +
    'hidden, so the resume was refused, and nothing retried it. The card sits ' +
    'frozen with no banner and a healthy router — the reported bug');
  assert.equal(moves[moves.length - 1], 'unpause',
    'the last thing done to the diagram was a pause, so it is standing still');
});

check('a socket outage that ends while the tab is hidden resumes on return', () => {
  const { moves, banners, setHidden } = mount();
  banners.onSocketConnect();
  banners.setRosBanner(true);
  moves.length = 0;

  banners.onSocketDisconnect();
  assert.deepEqual(moves, ['pause'], 'losing the socket did not pause the diagram');

  setHidden(true);
  banners.onSocketConnect();            // the backoff timer keeps firing when hidden
  setHidden(false);

  assert.equal(moves[moves.length - 1], 'unpause',
    'a socket that reconnected while the tab was hidden left the diagram paused');
});

check('the tab returning while the router is still down does NOT resume', () => {
  const { moves, banners, setHidden } = mount();
  banners.onSocketConnect();
  banners.setRosBanner(false, 'RouterOS not connected');
  moves.length = 0;

  setHidden(true);
  setHidden(false);

  assert.ok(!moves.includes('unpause'),
    'coming back to the tab restarted the dots over an unreachable router, ' +
    'which is the one thing the pause exists to prevent');
});

check('the tab returning while the SOCKET is still down does NOT resume', () => {
  const { moves, banners, setHidden } = mount();
  banners.onSocketDisconnect();
  moves.length = 0;

  setHidden(true);
  setHidden(false);

  assert.ok(!moves.includes('unpause'),
    'the dots restarted while the browser had no socket at all; the CSS happens ' +
    'to hide them, which is cover rather than correctness');
});

check('hiding the tab pauses', () => {
  const { moves, banners, setHidden } = mount();
  banners.onSocketConnect();
  banners.setRosBanner(true);
  moves.length = 0;

  setHidden(true);

  assert.deepEqual(moves, ['pause'],
    'a hidden tab kept animating — the half that pairs with the resume guard');
});

// ── AND THAT SOMETHING CALLS IT ────────────────────────────────────────────
//
// Source-level on purpose, and the same technique as the `switchRouter` checks
// in dashboard-wiring.test.ts: booting main.ts needs the whole page, so there is
// no cheap way to observe the effect. A perfectly wired banners.ts that nothing
// initialises is the bug in a different place.
check('wireBanners initialises the visibility handler', () => {
  const ts = require(path.join(ROOT, 'web', 'node_modules', 'typescript'));
  const mainPath = path.join(ROOT, 'web', 'src', 'main.ts');
  const sf = ts.createSourceFile(mainPath, fs.readFileSync(mainPath, 'utf8'),
    ts.ScriptTarget.ES2022, true);

  let body: any = null;
  const findFn = (n: any) => {
    if (ts.isFunctionDeclaration(n) && n.name && n.name.text === 'wireBanners') body = n;
    ts.forEachChild(n, findFn);
  };
  ts.forEachChild(sf, findFn);
  assert.ok(body, 'main.ts has no wireBanners function');

  const calls = new Set<string>();
  const walk = (n: any) => {
    if (ts.isCallExpression(n) && ts.isIdentifier(n.expression)) calls.add(n.expression.text);
    ts.forEachChild(n, walk);
  };
  walk(body);
  assert.ok(calls.has('initDiagramVisibility'),
    'wireBanners does not call initDiagramVisibility, so nothing binds ' +
    'visibilitychange and the diagram has no way to resume after a hidden outage');
});

if (failed) { say('\n' + failed + ' failed'); process.exit(1); }
say('\nall passed');
