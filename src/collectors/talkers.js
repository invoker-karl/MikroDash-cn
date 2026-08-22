/**
 * Top Talkers (Kid Control) — streams /ip/kid-control/device/print.
 *
 * Uses ros.stream() with null callback + 'data' event to bypass RStream's
 * section-handling debounce. Kid Control exposes traffic counters through its
 * `print stats` view; the API equivalent is the empty `=stats=` attribute.
 * Prefer rate-up/rate-down when present and fall back to bytes-up/bytes-down
 * deltas because RouterOS versions do not expose an identical stats schema.
 *
 * A 300 ms debounce accumulates per-device packets from each interval tick
 * before processing (RouterOS sends one !re per device per tick in a burst).
 *
 * Error classification:
 *   "unknown command" / "no such" → feature not present on this router;
 *     latch _unavailable, emit { devices: [], available: false } once, and stop.
 *     The latch is cleared ONLY by a deliberate re-probe — ros 'connected' or
 *     probe() — never by a later successful tick, and _startStream()/resume()
 *     both honour it, so an idle wake-up cannot quietly reopen the channel.
 *   "timeout" in stream mode → CHR/VM thread starvation; auto-downgrade to
 *     poll mode and restart. If poll also fails it goes through the poll
 *     handler below.
 *   "timeout" in poll mode → transient; log and retry normally.
 *   other stream errors → exponential backoff, retry stream.
 */

const { clampPoll, stopStreamSafe } = require('./util');
const { AuthoritativeSnapshotProbe, classifyRStreamPacket, classifySnapshotError } = require('./rstreamSnapshot');

const TALKERS_CMD = '/ip/kid-control/device/print';
const STATS_ARGS = ['=stats='];

function parseScaled(value, units) {
  if (value === null || value === undefined || value === '') return null;
  if (typeof value === 'number') return Number.isFinite(value) ? value : null;
  const text = String(value).trim();
  if (/^[+-]?(?:\d+\.?\d*|\.\d+)$/.test(text)) return Number(text);
  const match = text.match(/^([+-]?(?:\d+\.?\d*|\.\d+))\s*([KMGT]?i?B|[KMGT]?bps)$/i);
  if (!match) return null;
  const factor = units[match[2].toLowerCase()];
  return factor ? Number(match[1]) * factor : null;
}

function parseRateBps(value) {
  return parseScaled(value, {
    bps: 1, kbps: 1e3, mbps: 1e6, gbps: 1e9, tbps: 1e12,
  });
}

function parseBytes(value) {
  return parseScaled(value, {
    b: 1, kb: 1e3, mb: 1e6, gb: 1e9, tb: 1e12,
    kib: 1024, mib: 1024 ** 2, gib: 1024 ** 3, tib: 1024 ** 4,
  });
}

class TopTalkersCollector {
  constructor({ ros, io, pollMs, state, topN, streamMode, connectionStaleMs }) {
    this.ros    = ros;
    this.io     = io;
    this._lbl   = ros.routerLabel ? `[${ros.routerLabel}][talkers]` : '[talkers]';
    this.pollMs = pollMs;
    this._pollDelayMs = clampPoll(pollMs, 3000);
    this.state  = state;
    this.topN   = topN || 5;
    this.streamMode = streamMode !== false; // default true
    this.lastPayload = null;

    this._stream      = null;
    this._devicesNext = new Map(); // mac -> { name, mac, rateUp, rateDown }
    this._commitTimer = null;
    this._backoffTimer = null;
    this._backoffUntil = 0;
    this._baseBackoffMs = 60000;
    this._maxBackoffMs  = 600000;              // 10-minute cap
    this._backoffMs    = this._baseBackoffMs;  // doubles on each failure
    this._unavailable  = false;
    this._lastFp       = '';
    this._pollTimer    = null;
    this._pollInflight = false;
    this._counterPrev  = new Map();
    this._kidPayload   = null;
    this._connectionPayload = null;
    this._connectionTimer = null;
    this._connectionStaleMs = Number.isFinite(connectionStaleMs) ? Math.max(10, connectionStaleMs) : null;
    this._lastEmitTs = 0;
    this._active = false;
    this._snapshotProbe = new AuthoritativeSnapshotProbe({
      cooldownMs: Math.max(1000, this._pollDelayMs),
      // This must use the same stats view as the stream. A plain print may
      // legitimately return [] while stats still contains active devices, so
      // plain print is not an authoritative empty for this collector.
      read: () => this._readStatsSnapshot(),
      apply: rows => this._replaceAuthoritativeRows(rows),
      onError: (error, classification) => this._handleSnapshotError(error, classification),
    });
    this._heartbeatTimer = null;
    this._heartbeatArmedMs = 0;
    this._silenceTimer = null;
    this._sawData      = false;

    // Register lifecycle listeners once in the constructor so they never
    // accumulate across multiple start() calls (hot-swap safety).
    io.on('connection', () => {
      if (this._active && this.streamMode && !this._stream && !this._unavailable) this._startStream();
    });
    ros.on('close', () => {
      this._clearConnectionPayload(false);
      this._stopStream();
      // Stop the re-emit too: replaying a payload from before the disconnect would
      // keep the card looking live while the router is unreachable.
      this._stopHeartbeat();
      this._stopSilenceTimer();
      if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
    });
    ros.on('connected', () => {
      this._backoffUntil = 0;
      this._backoffMs    = this._baseBackoffMs;
      // A reconnect is a deliberate re-probe: the router may have been upgraded
      // or had the kid-control package added, so the latch is cleared here — and
      // only here, plus the dormancy probe. Never on an ordinary successful tick.
      this._unavailable  = false;
      this._lastFp       = '';
      this._kidPayload   = null;
      this._clearConnectionPayload(false);
      clearTimeout(this._backoffTimer);
      this._backoffTimer = null;
      this._stream = null;
      if (this._active) this._startTalkers();
    });
  }

  _startStream() {
    if (this._stream) return;
    if (this._connectionPayload) return;
    if (!this.ros.connected) return;
    if (this._unavailable) return;
    if (Date.now() < this._backoffUntil) return;

    const intervalSec = Math.max(1, Math.round(this.pollMs / 1000));
    console.log('%s', this._lbl + ' streaming /ip/kid-control/device/print, interval=' + intervalSec + 's');

    const stream = this.ros.stream(
      TALKERS_CMD,
      [
        '=stats=',
        `=interval=${intervalSec}`,
      ],
      null
    );

    stream.on('data', (packet) => {
      const classified = classifyRStreamPacket(packet);
      if (classified.kind === 'idle') { this._snapshotProbe.onIdle(); return; }
      if (classified.kind !== 'data') return;
      this._sawData = true;
      this._snapshotProbe.noteRealRow();
      const device = this._normaliseDevice(packet);
      if (!device) return;
      this._devicesNext.set(device.key, device);
      this._scheduleCommit();
    });

    stream.on('error', (err) => {
      const msg = String(err && err.message ? err.message : err);
      // Stop the actual errored stream before clearing our reference. Clearing
      // first made _stopStream() return early and leaked the RouterOS stream.
      this._stopStream();
      if (msg.includes('unknown command') || msg.includes('no such')) {
        // Feature not present on this router — disable permanently, no retries.
        console.warn('%s', this._lbl + ' Kid Control not available on this router — disabling');
        this._setUnavailable('Kid Control is unavailable');
      } else if (/not permitted|not allowed|permission denied|not enough privileges|cannot run/i.test(msg)) {
        console.warn('%s', this._lbl + ' Kid Control permission denied — disabling until reconnect');
        this._setUnavailable('Kid Control permission denied');
      } else if (msg.includes('timeout')) {
        // Stream timeout on CHR/VM (limited API threads). Feature likely exists
        // but stream mode can't handle it — auto-downgrade to poll mode.
        console.warn('%s', this._lbl + ' stream timeout — switching to poll mode');
        this.streamMode = false;
        this._startTalkers();
      } else {
        console.error('%s', this._lbl + ' stream error:', msg);
        this.state.lastTalkersErr = msg;
        clearTimeout(this._backoffTimer);
        const delay = this._backoffMs;
        this._backoffMs = Math.min(this._backoffMs * 2, this._maxBackoffMs);
        this._backoffTimer = setTimeout(() => { this._backoffTimer = null; this._startStream(); }, delay);
      }
    });

    this._stream = stream;
  }

  // Every other streamed collector re-emits its last payload every 60 s so the
  // browser's stale timer never fires on a healthy but quiet router. Talkers was
  // the one that did not, while still advertising a poll interval — so the client
  // held it to a 23 s threshold that nothing was ever going to meet once the
  // device list stopped changing.
  // What the CLIENT is told, not what we schedule on. A streamed collector reports
  // 0 so the browser keeps its fixed stale threshold and lets the heartbeat set the
  // cadence; advertising a 3 s interval while streaming is what held this card to a
  // 23 s deadline nothing was going to meet. (queues/wan idiom.)
  //
  // A getter, not a field: the stream-timeout path flips streamMode at runtime, and
  // a value captured in the constructor would then describe the wrong delivery mode
  // for the rest of the session.
  get _reportedPollMs() { return this.streamMode ? 0 : this.pollMs; }

  // The heartbeat only does its job if it beats faster than the deadline this
  // collector itself advertises. The client sets its threshold to
  // `pollMs + STALE_GRACE(20 s)` for a polled collector, and keeps a fixed 90 s
  // for a streamed one (pollMs 0) — so a hardcoded 60 s is right for streaming
  // and useless for polling, where a 3 s interval means a 23 s deadline. That is
  // the hAP AC2 case: it runs collection mode "poll", so the card still went
  // stale ~30 s in, and only recovered when dormancy fired ~20 s after that.
  get _heartbeatMs() { return this.streamMode ? 60000 : Math.max(5000, this.pollMs); }

  /**
   * Treat prolonged silence on an open stream as the empty answer.
   *
   * RouterOS replies to an empty result set with `!empty`, and patch-routeros.js
   * deliberately SWALLOWS that on a streaming channel, because there it means
   * "nothing YET" rather than "nothing" (/interface/wifi/frequency-scan sends it
   * ~6 ms before delivering real rows ten seconds later). So on a router whose
   * kid-control table is empty, a stream-mode collector receives no packet at
   * all — not even the `[]` the data handler above is written for.
   *
   * That is why this looked fixed on the hAP AC2 and was not on the cAP AX: the
   * AC2 runs collection mode "poll", where ros.write() goes down a one-shot
   * channel and the same patch DOES turn `!empty` into an empty result.
   *
   * Silence schedules the same authoritative one-shot confirmation used for an
   * RStream idle marker. That preserves the Chinese edition's last-good contract:
   * only a successful ordinary print may clear the final row, while a transient
   * confirmation error keeps the last known result.
   */
  _startSilenceTimer() {
    if (this._silenceTimer) return;
    const intervalMs = Math.max(1000, this.pollMs);
    this._silenceTimer = setInterval(() => {
      if (!this.streamMode || !this._stream || this._unavailable) return;
      if (this._sawData) { this._sawData = false; return; }   // rows arrived; nothing to infer
      if (this._devicesNext.size > 0) return;                 // mid-batch, let the debounce commit
      this._snapshotProbe.onIdle();
    }, intervalMs * 3);
    if (this._silenceTimer.unref) this._silenceTimer.unref();
  }

  _stopSilenceTimer() {
    if (this._silenceTimer) { clearInterval(this._silenceTimer); this._silenceTimer = null; }
    this._sawData = false;
  }

  _startHeartbeat() {
    // Re-arm when the cadence changes: the stream-timeout path flips streamMode
    // and calls _startTalkers() again, which would otherwise hit the early return
    // below and leave a 60 s beat guarding a 23 s deadline.
    if (this._heartbeatTimer && this._heartbeatArmedMs !== this._heartbeatMs) this._stopHeartbeat();
    if (this._heartbeatTimer) return;
    this._heartbeatArmedMs = this._heartbeatMs;
    this._heartbeatTimer = setInterval(() => {
      if (!this.lastPayload) return;
      // Idle gate, as netwatch does: this re-emit exists only for browser stale
      // timers, so it has nothing to do when nobody is watching.
      if (this.io.engine.clientsCount === 0) return;
      this.io.to('page-dashboard').emit('talkers:update', { ...this.lastPayload, ts: Date.now() });
    }, this._heartbeatMs);
    if (this._heartbeatTimer.unref) this._heartbeatTimer.unref();
  }

  _stopHeartbeat() {
    if (this._heartbeatTimer) { clearInterval(this._heartbeatTimer); this._heartbeatTimer = null; }
    this._heartbeatArmedMs = 0;
  }

  _stopStream() {
    this._snapshotProbe.invalidate();
    clearTimeout(this._commitTimer);  this._commitTimer  = null;
    clearTimeout(this._backoffTimer); this._backoffTimer = null;
    this._devicesNext.clear();
    this._counterPrev.clear();
    if (!this._stream) return;
    stopStreamSafe(this._stream);
    this._stream = null;
  }

  _restartStream() {
    this._stopStream();
    if (!this._connectionPayload) this._startStream();
  }

  _scheduleCommit() {
    clearTimeout(this._commitTimer);
    this._commitTimer = setTimeout(() => this._commitTick(), 300);
  }

  _readStatsSnapshot() {
    // Deliberately omit .proplist. Kid Control stats fields have varied across
    // RouterOS releases, and this table is small; an over-tight field list can
    // remove the only stable identity or the bytes fallback.
    return this.ros.write(TALKERS_CMD, STATS_ARGS.slice());
  }

  _normaliseDevice(row, now = Date.now()) {
    if (!row || typeof row !== 'object') return null;
    const mac = String(row['mac-address'] || '').trim();
    const id = String(row['.id'] || '').trim();
    const name = String(row.name || '').trim();
    const key = mac || id;
    if (!key) return null;

    const bytesUp = parseBytes(row['bytes-up']);
    const bytesDown = parseBytes(row['bytes-down']);
    const previous = this._counterPrev.get(key);
    let rateUp = parseRateBps(row['rate-up']);
    let rateDown = parseRateBps(row['rate-down']);
    if (previous && now > previous.ts) {
      const seconds = (now - previous.ts) / 1000;
      if (rateUp === null && bytesUp !== null && previous.up !== null) {
        rateUp = Math.max(0, (bytesUp - previous.up) * 8 / seconds);
      }
      if (rateDown === null && bytesDown !== null && previous.down !== null) {
        rateDown = Math.max(0, (bytesDown - previous.down) * 8 / seconds);
      }
    }
    if (bytesUp !== null || bytesDown !== null) {
      this._counterPrev.set(key, { up: bytesUp, down: bytesDown, ts: now });
    }
    return {
      key,
      // RouterOS .id is an internal implementation key, not a device label.
      // Never expose it through the public name fallback.
      name: name || mac || 'Unknown device',
      mac,
      rateUp: rateUp === null ? 0 : rateUp,
      rateDown: rateDown === null ? 0 : rateDown,
    };
  }

  _replaceAuthoritativeRows(rows) {
    const next = new Map();
    for (const row of rows) {
      const device = this._normaliseDevice(row);
      if (device) next.set(device.key, device);
    }
    this._devicesNext = next;
    const live = new Set(next.keys());
    for (const key of this._counterPrev.keys()) if (!live.has(key)) this._counterPrev.delete(key);
    this._commitTick();
  }

  _setUnavailable(reason) {
    this._unavailable = true;
    const now = Date.now();
    this._kidPayload = {
      ts: now, devices: [], pollMs: this._reportedPollMs, available: false,
      unavailable: true, reason,
      source: 'kid-control', basis: 'kid-control-stats', status: 'error',
      stale: false, emptyText: null,
    };
    this._publishSelected();
  }

  _handleSnapshotError(error, classification = classifySnapshotError(error)) {
    if (classification.kind === 'unsupported') {
      this._stopStream();
      this._setUnavailable('Kid Control is unavailable');
    } else if (classification.kind === 'permission') {
      this._stopStream();
      this._setUnavailable('Kid Control permission denied');
    } else {
      this.state.lastTalkersErr = classification.message;
    }
  }

  _commitTick() {
    this._commitTimer  = null;
    // Reset the retry backoff on success, but NOT the _unavailable latch: a
    // successful tick means the stream recovered, never that a router which
    // answered "unknown command" has grown a kid-control menu. Clearing it here
    // un-latched the feature probe almost immediately.
    this._backoffMs    = this._baseBackoffMs;
    const now = Date.now();

    if (this.io.engine.clientsCount === 0) {
      this._devicesNext.clear();
      this._stopStream();
      return;
    }

    let devices = [...this._devicesNext.values()].map(d => ({
      key:     d.key || d.mac,
      name:    d.name,
      mac:     d.mac,
      tx_mbps: +(d.rateUp   / 1_000_000).toFixed(3),
      rx_mbps: +(d.rateDown / 1_000_000).toFixed(3),
    }));
    this._devicesNext.clear();

    devices.sort((a, b) => (b.rx_mbps + b.tx_mbps) - (a.rx_mbps + a.tx_mbps));
    devices = devices.slice(0, this.topN);

    const fp = JSON.stringify(devices.map(d => ({ key: d.key, tx: d.tx_mbps, rx: d.rx_mbps })));
    const publicDevices = devices.map(({ key: _key, ...device }) => device);
    this._kidPayload = {
      ts: now, devices: publicDevices, pollMs: this._reportedPollMs, available: true,
      unavailable: false, reason: null, source: 'kid-control', basis: 'kid-control-stats',
      status: 'ok', stale: false, emptyText: null,
      _fp: fp,
    };
    this._publishSelected();
  }

  /**
   * Accept the already-computed per-LAN-device rates from BandwidthCollector.
   * This is the preferred dashboard source because it covers ordinary LAN
   * clients without requiring RouterOS parental-control configuration.
   * null means that source stopped or became unavailable; Kid Control then
   * resumes as a compatibility fallback.
   */
  acceptConnectionPayload(payload) {
    if (!payload) {
      this._clearConnectionPayload(true);
      return;
    }
    if (!Array.isArray(payload.devices)) return;

    const byIdentity = new Map();
    for (const row of payload.devices) {
      if (!row || typeof row !== 'object') continue;
      const srcIp = String(row.srcIp || '').trim();
      if (!srcIp) continue;
      const rx = Number(row.rxMbps);
      const tx = Number(row.txMbps);
      const mac = String(row.mac || '').trim().toUpperCase().replace(/-/g, ':');
      const key = mac ? `mac:${mac}` : `ip:${srcIp}`;
      const current = byIdentity.get(key) || {
        name: String(row.name || mac || srcIp), mac,
        rx_mbps: 0, tx_mbps: 0, _key: key,
      };
      current.rx_mbps += Number.isFinite(rx) && rx >= 0 ? rx : 0;
      current.tx_mbps += Number.isFinite(tx) && tx >= 0 ? tx : 0;
      if ((!current.name || current.name === mac) && row.name) current.name = String(row.name);
      byIdentity.set(key, current);
    }
    const devices = [...byIdentity.values()].map(device => ({
      ...device,
      rx_mbps: +device.rx_mbps.toFixed(4),
      tx_mbps: +device.tx_mbps.toFixed(4),
    }));
    devices.sort((a, b) =>
      ((b.rx_mbps + b.tx_mbps) - (a.rx_mbps + a.tx_mbps)) || a._key.localeCompare(b._key));
    const selected = devices.slice(0, this.topN);
    const publicDevices = selected.map(({ _key, ...device }) => device);
    const ts = Number.isFinite(payload.ts) && payload.ts > 0 ? payload.ts : Date.now();
    const pollMs = Number.isFinite(payload.pollMs) && payload.pollMs > 0 ? payload.pollMs : 5000;
    this._connectionPayload = {
      ts, devices: publicDevices, pollMs, available: true, unavailable: false, reason: null,
      source: 'connections', basis: 'connection-byte-delta', status: 'ok',
      stale: false, emptyText: 'No active LAN devices',
      _fp: JSON.stringify(selected.map(d => ({ key: d._key, rx: d.rx_mbps, tx: d.tx_mbps }))),
    };

    // The connection-derived source is authoritative even when it is empty.
    // Stop the redundant Kid Control command while this source is fresh.
    // Do not retain an older Kid Control payload: if the preferred source later
    // disappears, replaying that payload would make stale devices look current.
    this._kidPayload = null;
    this._stopStream();
    this._stopSilenceTimer();
    if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
    clearTimeout(this._connectionTimer);
    const staleMs = this._connectionStaleMs || Math.max(20000, pollMs * 4 + 5000);
    this._connectionTimer = setTimeout(() => {
      this._connectionTimer = null;
      this._clearConnectionPayload(true);
    }, staleMs);
    if (this._connectionTimer.unref) this._connectionTimer.unref();
    this._publishSelected();
  }

  _clearConnectionPayload(restartFallback) {
    clearTimeout(this._connectionTimer);
    this._connectionTimer = null;
    const hadConnection = !!this._connectionPayload;
    this._connectionPayload = null;
    if (hadConnection && restartFallback) {
      const now = Date.now();
      this._kidPayload = {
        ts: now, devices: [], pollMs: this._reportedPollMs, available: true, unavailable: true,
        reason: 'Device traffic is unavailable', source: 'fallback-pending',
        basis: null, status: 'error', stale: true, emptyText: null,
      };
      this._publishSelected();
    }
    if (restartFallback && this._active && this.ros.connected) this._startTalkers();
  }

  _publishSelected() {
    const internal = this._connectionPayload || this._kidPayload;
    if (!internal) return;
    const { _fp, ...payload } = internal;
    this.lastPayload = payload;
    const fp = JSON.stringify({
      source: payload.source,
      unavailable: !!payload.unavailable,
      reason: payload.reason || '',
      devices: _fp || payload.devices,
    });
    const now = Date.now();
    if (fp !== this._lastFp || now - this._lastEmitTs >= 10000) {
      this._lastFp = fp;
      this._lastEmitTs = now;
      this.io.to('page-dashboard').emit('talkers:update', payload);
    }
    this.state.lastTalkersTs = payload.ts || now;
    this.state.lastTalkersErr = null;
  }

  // ── poll-mode talkers path ────────────────────────────────────────────────

  async _pollTalkersOnce() {
    if (!this.ros.connected || this._pollInflight) return;
    if (this.io.engine.clientsCount === 0) return;
    this._pollInflight = true;
    try {
      const rows = await this._readStatsSnapshot();
      this._replaceAuthoritativeRows(Array.isArray(rows) ? rows : []);
    } catch (e) {
      const msg = String(e && e.message ? e.message : e);
      if (msg.includes('unknown command') || msg.includes('no such')) {
        // Feature not present — disable permanently, stop scheduling.
        if (!this._unavailable) {
          console.warn('%s', this._lbl + ' poll: Kid Control not available — disabling');
          this._setUnavailable('Kid Control is unavailable');
        }
      } else if (/not permitted|not allowed|permission denied|not enough privileges|cannot run/i.test(msg)) {
        if (!this._unavailable) this._setUnavailable('Kid Control permission denied');
      } else {
        // Timeout or other transient error — log, let normal scheduling continue.
        this.state.lastTalkersErr = msg;
      }
    } finally {
      this._pollInflight = false;
    }
  }

  _scheduleTalkersNext() {
    if (this._unavailable) return;
    clearTimeout(this._pollTimer);
    this._pollTimer = setTimeout(async () => {
      this._pollTimer = null;
      if (!this.streamMode) {
        await this._pollTalkersOnce();
        this._scheduleTalkersNext();
      }
    }, this._pollDelayMs);
  }

  _startTalkers() {
    if (!this._active) return;
    this._startHeartbeat();
    if (this._connectionPayload) return;
    if (this.streamMode) {
      this._startSilenceTimer();
      this._startStream();
    } else {
      this._stopSilenceTimer();
      console.log('%s', this._lbl + ' poll mode — polling /ip/kid-control/device/print every', this.pollMs + 'ms');
      this._pollTalkersOnce();
      this._scheduleTalkersNext();
    }
  }

  start() {
    this._active = true;
    this._startTalkers();
  }

  suspend() {
    this._active = false;
    this._clearConnectionPayload(false);
    this._stopStream();
    this._stopHeartbeat();
    this._stopSilenceTimer();
    if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
  }
  resume() {
    if (!this.ros.connected) return;
    this._active = true;
    // Latched off: resume() is an idle/page-gate wake-up, not a feature re-probe.
    // Without this, every socket reconnect re-opened the stream on a router that
    // had already answered "unknown command".
    if (this._unavailable) return;
    // Same trap as ping: suspend() clears _pollTimer, so a resume that only
    // restarts the stream strands poll mode permanently once the last viewer
    // has ever gone away — which is why the Top Talkers card stayed stale.
    this._startTalkers();
  }

  // Deliberate feature re-probe, as opposed to resume()'s idle wake-up: clears
  // the unsupported latch and the retry backoff, then reopens the channel. The
  // dormancy supervisor calls this on backoff expiry and on page focus.
  probe() {
    this._unavailable  = false;
    this._backoffUntil = 0;
    this._backoffMs    = this._baseBackoffMs;
    this._lastFp       = '';
    this.resume();
  }

  stop() {
    this._active = false;
    this._clearConnectionPayload(false);
    this._stopStream();
    this._stopHeartbeat();
    this._stopSilenceTimer();
    if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
  }
}

module.exports = TopTalkersCollector;
