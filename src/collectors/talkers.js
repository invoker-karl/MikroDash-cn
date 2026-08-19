/**
 * Top Talkers (Kid Control) — streams /ip/kid-control/device/print.
 *
 * Uses ros.stream() with null callback + 'data' event to bypass RStream's
 * section-handling debounce. RouterOS delivers rate-up / rate-down
 * (bytes/second) directly — no byte-delta calculation needed.
 *
 * A 300 ms debounce accumulates per-device packets from each interval tick
 * before processing (RouterOS sends one !re per device per tick in a burst).
 *
 * Error classification:
 *   "unknown command" / "no such" → feature not present on this router;
 *     disable permanently (no retries, empty payload, silent card).
 *   "timeout" in stream mode → CHR/VM thread starvation; auto-downgrade to
 *     poll mode and restart. If poll also fails it goes through the poll
 *     handler below.
 *   "timeout" in poll mode → transient; log and retry normally.
 *   other stream errors → exponential backoff, retry stream.
 */

const { clampPoll, stopStreamSafe } = require('./util');
const { AuthoritativeSnapshotProbe, classifyRStreamPacket, classifySnapshotError } = require('./rstreamSnapshot');

class TopTalkersCollector {
  constructor({ ros, io, pollMs, state, topN, streamMode }) {
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
    this._backoffMs    = 60000;
    this._unavailable  = false;
    this._lastFp       = '';
    this._pollTimer    = null;
    this._pollInflight = false;
    this._snapshotProbe = new AuthoritativeSnapshotProbe({
      cooldownMs: Math.max(1000, this._pollDelayMs),
      read: () => this.ros.write('/ip/kid-control/device/print', [
        '=.proplist=name,mac-address,rate-up,rate-down',
      ]),
      apply: rows => this._replaceAuthoritativeRows(rows),
      onError: (error, classification) => this._handleSnapshotError(error, classification),
    });

    // Register lifecycle listeners once in the constructor so they never
    // accumulate across multiple start() calls (hot-swap safety).
    io.on('connection', () => {
      if (this.streamMode && !this._stream) this._startStream();
    });
    ros.on('close', () => {
      this._stopStream();
      if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
    });
    ros.on('connected', () => {
      this._backoffUntil = 0;
      this._backoffMs    = 60000;
      this._unavailable  = false;
      this._lastFp       = '';
      clearTimeout(this._backoffTimer);
      this._backoffTimer = null;
      this._stream = null;
      this._startTalkers();
    });
  }

  _startStream() {
    if (this._stream) return;
    if (!this.ros.connected) return;
    if (Date.now() < this._backoffUntil) return;

    const intervalSec = Math.max(1, Math.round(this.pollMs / 1000));
    console.log('%s', this._lbl + ' streaming /ip/kid-control/device/print, interval=' + intervalSec + 's');

    const stream = this.ros.stream(
      '/ip/kid-control/device/print',
      [
        `=interval=${intervalSec}`,
        '=.proplist=name,mac-address,rate-up,rate-down',
      ],
      null
    );

    stream.on('data', (packet) => {
      const classified = classifyRStreamPacket(packet);
      if (classified.kind === 'idle') { this._snapshotProbe.onIdle(); return; }
      if (classified.kind !== 'data') return;
      this._snapshotProbe.noteRealRow();
      const mac = packet['mac-address'];
      if (!mac) return;
      this._devicesNext.set(mac, {
        name:     packet.name || '',
        mac,
        rateUp:   parseInt(packet['rate-up']   || '0', 10),
        rateDown: parseInt(packet['rate-down'] || '0', 10),
      });
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
        this._backoffTimer = setTimeout(() => { this._backoffTimer = null; this._startStream(); }, this._backoffMs);
      }
    });

    this._stream = stream;
  }

  _stopStream() {
    this._snapshotProbe.invalidate();
    clearTimeout(this._commitTimer);  this._commitTimer  = null;
    clearTimeout(this._backoffTimer); this._backoffTimer = null;
    this._devicesNext.clear();
    if (!this._stream) return;
    stopStreamSafe(this._stream);
    this._stream = null;
  }

  _restartStream() {
    this._stopStream();
    this._startStream();
  }

  _scheduleCommit() {
    clearTimeout(this._commitTimer);
    this._commitTimer = setTimeout(() => this._commitTick(), 300);
  }

  _replaceAuthoritativeRows(rows) {
    this._devicesNext.clear();
    for (const row of rows) {
      if (!row || typeof row !== 'object') continue;
      const mac = row['mac-address'];
      if (!mac) continue;
      this._devicesNext.set(mac, {
        name: row.name || '', mac,
        rateUp: parseInt(row['rate-up'] || '0', 10),
        rateDown: parseInt(row['rate-down'] || '0', 10),
      });
    }
    this._commitTick();
  }

  _setUnavailable(reason) {
    this._unavailable = true;
    const now = Date.now();
    const payload = { ts: now, devices: [], pollMs: this.pollMs, unavailable: true, reason };
    this.lastPayload = payload;
    this._lastFp = 'unavailable:' + reason;
    this.io.to('page-dashboard').emit('talkers:update', payload);
    this.state.lastTalkersTs = now;
    this.state.lastTalkersErr = null;
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
    this._backoffMs    = 60000;
    this._unavailable  = false;
    const now = Date.now();

    if (this.io.engine.clientsCount === 0) {
      this._devicesNext.clear();
      this._stopStream();
      return;
    }

    let devices = [...this._devicesNext.values()].map(d => ({
      name:    d.name,
      mac:     d.mac,
      tx_mbps: +(d.rateUp   / 1_000_000).toFixed(3),
      rx_mbps: +(d.rateDown / 1_000_000).toFixed(3),
    }));
    this._devicesNext.clear();

    devices.sort((a, b) => (b.rx_mbps + b.tx_mbps) - (a.rx_mbps + a.tx_mbps));
    devices = devices.slice(0, this.topN);

    const fp = JSON.stringify(devices.map(d => ({ mac: d.mac, tx: d.tx_mbps, rx: d.rx_mbps })));
    this.lastPayload = { ts: now, devices, pollMs: this.pollMs, unavailable: false, reason: null };
    if (fp !== this._lastFp) {
      this._lastFp = fp;
      this.io.to('page-dashboard').emit('talkers:update', this.lastPayload);
    }
    this.state.lastTalkersTs  = now;
    this.state.lastTalkersErr = null;
  }

  // ── poll-mode talkers path ────────────────────────────────────────────────

  async _pollTalkersOnce() {
    if (!this.ros.connected || this._pollInflight) return;
    if (this.io.engine.clientsCount === 0) return;
    this._pollInflight = true;
    try {
      const rows = await this.ros.write('/ip/kid-control/device/print', [
        '=.proplist=name,mac-address,rate-up,rate-down',
      ]);
      this._devicesNext.clear();
      if (Array.isArray(rows)) {
        for (const r of rows) {
          const mac = r['mac-address'];
          if (!mac) continue;
          this._devicesNext.set(mac, {
            name:     r.name || '',
            mac,
            rateUp:   parseInt(r['rate-up']   || '0', 10),
            rateDown: parseInt(r['rate-down'] || '0', 10),
          });
        }
      }
      this._commitTick();
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
    if (this.streamMode) {
      this._startStream();
    } else {
      console.log('%s', this._lbl + ' poll mode — polling /ip/kid-control/device/print every', this.pollMs + 'ms');
      this._pollTalkersOnce();
      this._scheduleTalkersNext();
    }
  }

  start() {
    this._startTalkers();
  }

  suspend() {
    this._stopStream();
    if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
  }
  resume() {
    if (!this.ros.connected) return;
    // Same trap as ping: suspend() clears _pollTimer, so a resume that only
    // restarts the stream strands poll mode permanently once the last viewer
    // has ever gone away — which is why the Top Talkers card stayed stale.
    if (this.streamMode) { this._startStream(); return; }
    this._pollTalkersOnce();
    this._scheduleTalkersNext();
  }

  stop() {
    this._stopStream();
    if (this._pollTimer) { clearTimeout(this._pollTimer); this._pollTimer = null; }
  }
}

module.exports = TopTalkersCollector;
