/**
 * Traffic collector — streams /interface/monitor-traffic =interface=<list> =interval=1.
 *
 * The stream covers only the union of interfaces currently being watched by
 * connected clients, plus defaultIf (always included for the WAN status badge).
 * When subscriptions change (traffic:select, disconnect) the stream is restarted
 * with the updated list.  This keeps RouterOS API load proportional to what is
 * actually being watched rather than the total interface count.
 *
 * setAvailableInterfaces() supplies the authoritative whitelist used by both
 * input validation and the consolidated stream.
 */
const RingBuffer = require('../util/ringbuffer');
const { parseBps, bpsToMbps, clampPoll, stopStreamSafe, createStreamHealth } = require('./util');

const MAX_INTERFACE_NAME_LENGTH = 128;

class TrafficCollector {
  constructor({ ros, io, defaultIf, historyMinutes, pollMs, state, onSample }) {
    this.ros        = ros;
    this.io         = io;
    this._lbl       = ros.routerLabel ? `[${ros.routerLabel}][traffic]` : '[traffic]';
    this.defaultIf  = defaultIf;
    this.state      = state;
    this._onSample  = onSample || null;
    // Contract metadata only — the stream interval is fixed at 1 s, and the
    // db-writer's MB accumulation assumes exactly one sample per second.
    this.pollMs     = clampPoll(pollMs, 1000);
    this.maxPoints  = Math.max(60, historyMinutes * 60);
    this.hist          = new Map();  // ifName -> RingBuffer
    this.subscriptions = new Map();  // socketId -> { ifName, socket }
    this._boundSockets = new Map();  // socketId -> { socket, onSelect, onDisconnect }
    this._allStream    = null;
    this._ifNamesKey   = '';         // sorted key of current stream — detects changes
    this.availableIfs  = new Set();  // validated set from fetchInterfaces()
    this._interfacesLoaded = false;
    this._loggedErr    = false;
    this._restartTimer = null;
    this._watchdogTimer = null;
    this._streamStartTs = 0;
    this._lastDataTs    = 0;
    this._configError   = null;
    this._configErrorSince = null;
    this.lastWanStatus = null;
    // Restarting a dead stream forever is correct recovery for a transient
    // stall, but it turns a persistent fault into a silent sawtooth: the chart
    // just grows holes and nothing says why. Track it and report it. (#106)
    this._health = createStreamHealth();
    this.lastHealth = null;

    this.ros.on('connected', () => {
      this._ifNamesKey = ''; // force restart on reconnect
      this._stopAllStream();
      this._ensureHistory(this.defaultIf);
      this._updateStream();
      this._startWatchdog();
    });
    this.ros.on('close', () => {
      this._stopAllStream();
      this._stopWatchdog();
    });
  }

  _ensureHistory(ifName) {
    if (!this.hist.has(ifName)) this.hist.set(ifName, new RingBuffer(this.maxPoints));
  }

  preloadHistory(ifName, rows) {
    if (!rows || !rows.length) return;
    this._ensureHistory(ifName);
    const buf = this.hist.get(ifName);
    for (const r of rows) buf.push({ ts: r.ts, rx_mbps: r.rx_mbps, tx_mbps: r.tx_mbps });
  }

  setAvailableInterfaces(interfaces) {
    const streamWasStarted = !!this._allStream || this._ifNamesKey !== '';
    const names = (interfaces || []).map(i => typeof i === 'string' ? i : i && i.name).filter(Boolean);
    this.availableIfs = new Set(names);
    this._interfacesLoaded = true;

    // A renamed/removed defaultIf used to poison the consolidated RouterOS
    // command even after the browser selected a valid interface:
    //   =interface=missing-default,lan
    // RouterOS rejects the whole command with "no such item", so lan never
    // receives a sample. Once the authoritative interface list is available,
    // keep invalid names out of the stream and expose the configuration fault.
    if (!this.availableIfs.has(this.defaultIf)) {
      const detail = `Configured default interface "${this.defaultIf}" is unavailable`;
      if (this._configError !== detail) {
        this._configError = detail;
        this._configErrorSince = Date.now();
        this.state.lastTrafficErr = detail;
        console.warn('%s', this._lbl + ' ' + detail);
        this._emitConfigHealth(true);
      }
    } else if (this._configError) {
      this._configError = null;
      this._configErrorSince = null;
      this.state.lastTrafficErr = null;
      this._emitConfigHealth(false);
    }

    // Loading the whitelist can remove an invalid name from a stream that was
    // optimistically opened during startup, so always re-evaluate the union.
    if (streamWasStarted) this._updateStream();
  }

  // Returns the sorted union of subscribed interfaces + defaultIf.
  _getStreamNames() {
    const s = new Set([this.defaultIf]);
    for (const { ifName } of this.subscriptions.values()) s.add(ifName);
    const names = [...s];
    // Before fetchInterfaces completes we retain the historical optimistic
    // startup. Afterwards the RouterOS list is authoritative and no stale name
    // may enter the consolidated command.
    return (this._interfacesLoaded ? names.filter(name => this.availableIfs.has(name)) : names).sort();
  }

  // Restart the stream only when the subscription set has changed.
  _updateStream() {
    const key = this._getStreamNames().join(',');
    if (key === this._ifNamesKey) return;
    this._ifNamesKey = key;
    this._stopAllStream();
    this._startAllStream();
  }

  _normalizeIfName(ifName) {
    if (typeof ifName !== 'string') return null;
    const trimmed = ifName.trim();
    if (!trimmed || trimmed.length > MAX_INTERFACE_NAME_LENGTH) return null;
    if (/[\r\n\0]/.test(trimmed)) return null;
    if (!this._interfacesLoaded) {
      console.warn('%s', this._lbl + ' traffic:select rejected — interface list not yet ready');
      return null;
    }
    if (!this.availableIfs.has(trimmed)) return null;
    return trimmed;
  }

  _stopAllStream() {
    if (this._restartTimer) { clearTimeout(this._restartTimer); this._restartTimer = null; }
    if (!this._allStream) {
      this._streamStartTs = 0;
      return;
    }
    stopStreamSafe(this._allStream);
    this._allStream = null;
    this._streamStartTs = 0;
    console.log('%s', this._lbl + ' stopped stream');
  }

  _startAllStream() {
    if (this._allStream) return;
    if (!this.ros.connected) return;

    const names = this._getStreamNames();
    // No valid interface is safer than sending =interface= and entering a
    // permanent restart loop. A browser selection (or refreshed interface
    // list) calls _updateStream() and starts the stream as soon as one is valid.
    if (!names.length) return;
    // start() opens the optimistic stream directly, outside _updateStream().
    // Record its real key so a later authoritative empty list can stop it.
    this._ifNamesKey = names.join(',');
    this._streamStartTs = Date.now();
    this._lastDataTs = 0;
    console.log('%s', this._lbl + ' streaming', names.length, 'interface(s) interval=1s');

    const stream = this.ros.stream(
      '/interface/monitor-traffic',
      [
        `=interface=${names.join(',')}`,
        '=interval=1',
        '=.proplist=name,rx-bits-per-second,tx-bits-per-second,running,disabled',
      ],
      null  // null callback — use 'data' event to bypass section-handling debounce
    );

    stream.on('data', (packet) => {
      if (!packet || typeof packet !== 'object' || Array.isArray(packet)) return;
      // When a single interface is monitored, RouterOS may omit the 'name' field.
      const ifName = packet.name || (names.length === 1 ? names[0] : null);
      if (!ifName) return;
      const hasRx = Object.prototype.hasOwnProperty.call(packet, 'rx-bits-per-second');
      const hasTx = Object.prototype.hasOwnProperty.call(packet, 'tx-bits-per-second');
      if (!hasRx && !hasTx) return;
      this._lastDataTs = Date.now();
      this._processPacket(ifName, packet);
    });

    stream.on('error', (err) => {
      const msg = err && err.message ? err.message : String(err);
      this._allStream = null;
      const isMissing = msg.includes('no such item');
      if (!this._loggedErr) {
        console.error('%s', this._lbl + ' stream error for [' + names.join(',') + ']:', msg);
        this._loggedErr = true;
      }
      // Do not hide "no such item": it is exactly the evidence needed to
      // distinguish a stale interface configuration from API starvation.
      this.state.lastTrafficErr = this._configError || msg;
      this._reportHealth(this._health.recordRestart());
      // Always schedule a restart — 'no such item' is a transient interface blip,
      // other errors may be CHR/VM killing the stream under resource pressure.
      if (this._restartTimer) clearTimeout(this._restartTimer); // overlapping errors must not leak timers
      this._restartTimer = setTimeout(() => {
        this._restartTimer = null;
        this._loggedErr = false;
        if (this.ros.connected && !this._allStream) this._startAllStream();
      }, isMissing ? 5000 : 3000);
    });

    this._allStream = stream;
  }

  // Emit only on a transition (createStreamHealth returns null otherwise), so a
  // degraded stream does not spam the socket every 5 s watchdog tick.
  _reportHealth(changed) {
    if (changed === null) return;
    const configDegraded = !!this._configError;
    this.lastHealth = {
      collector: 'traffic',
      degraded:  configDegraded || changed,
      restarts:  this._health.restarts,
      since:     configDegraded ? this._configErrorSince : (this._health.since || null),
      reason:    configDegraded ? 'Configured default interface is unavailable' : null,
      ts:        Date.now(),
    };
    console.warn('%s', this._lbl + (changed
      ? ' stream degraded — ' + this._health.restarts + ' restarts without recovery'
      : ' stream recovered'));
    this.io.emit('stream:health', this.lastHealth);
  }

  _emitConfigHealth(degraded) {
    // Configuration faults are immediately actionable and do not need three
    // watchdog cycles before becoming visible. Keep the user-facing reason
    // stable for i18n; the exact interface name remains in healthz and logs.
    const isDegraded = degraded || this._health.degraded;
    this.lastHealth = {
      collector: 'traffic',
      degraded: isDegraded,
      restarts: this._health.restarts,
      since: degraded ? this._configErrorSince : (isDegraded ? (this._health.since || Date.now()) : null),
      reason: degraded ? 'Configured default interface is unavailable' : null,
      ts: Date.now(),
    };
    this.io.emit('stream:health', this.lastHealth);
  }

  _startWatchdog() {
    this._stopWatchdog();
    const staleMs = 10000;
    this._watchdogTimer = setInterval(() => {
      if (!this.ros.connected || this._restartTimer) return;
      if (!this._allStream) {
        console.warn('%s', this._lbl + ' watchdog: stream missing — restarting');
        this._startAllStream();
        return;
      }
      const last = this._lastDataTs || this._streamStartTs;
      if (last && Date.now() - last > staleMs) {
        console.warn('%s', this._lbl + ' watchdog: no data for ' + Math.round((Date.now() - last) / 1000) + 's — restarting stream');
        this._reportHealth(this._health.recordRestart());
        this._stopAllStream();
        this._startAllStream();
      } else if (this._streamStartTs) {
        this._reportHealth(this._health.recordHealthy(Date.now() - this._streamStartTs));
      }
    }, 5000);
  }

  _stopWatchdog() {
    if (this._watchdogTimer) clearInterval(this._watchdogTimer);
    this._watchdogTimer = null;
  }

  bindSocket(socket) {
    this.subscriptions.set(socket.id, { ifName: this.defaultIf, socket });
    // defaultIf is always in the stream, so this is a no-op on first connect.
    this._updateStream();

    // bindSocket is called on connect, hot-swap replay and router:switch —
    // attach the listeners only once per socket or they stack.
    if (this._boundSockets.has(socket.id)) return;

    const onSelect = (payload) => {
      const nextIf = this._normalizeIfName(payload && payload.ifName);
      if (!nextIf) return;
      this.subscriptions.set(socket.id, { ifName: nextIf, socket });
      this._ensureHistory(nextIf);
      this._updateStream(); // expands stream to include nextIf if not already there
      socket.emit('traffic:history', {
        ifName: nextIf,
        points: this.hist.get(nextIf).toArray(),
      });
    };
    const onDisconnect = () => this.unbindSocket(socket);

    this._boundSockets.set(socket.id, { socket, onSelect, onDisconnect });
    socket.on('traffic:select', onSelect);
    socket.on('disconnect', onDisconnect);
  }

  // Detach listeners and drop the subscription — used on disconnect, on
  // router:switch (socket moves to another session's collector) and in stop().
  unbindSocket(socket) {
    const bound = this._boundSockets.get(socket.id);
    if (bound) {
      socket.off('traffic:select', bound.onSelect);
      socket.off('disconnect', bound.onDisconnect);
      this._boundSockets.delete(socket.id);
    }
    this.subscriptions.delete(socket.id);
    this._updateStream(); // shrinks stream if the interface is no longer subscribed
  }

  _processPacket(ifName, data) {
    const rxBps    = parseBps(data['rx-bits-per-second']);
    const txBps    = parseBps(data['tx-bits-per-second']);
    const running  = data.running  !== 'false' && data.running  !== false;
    const disabled = data.disabled === 'true'  || data.disabled === true;

    const now    = Date.now();
    const rxMbps = bpsToMbps(rxBps);
    const txMbps = bpsToMbps(txBps);

    // Always update WAN status regardless of idle state (cheap, needed for replay)
    if (ifName === this.defaultIf) {
      this.lastWanStatus = { ifName, ts: now, running, disabled };
    }

    // Always fire onSample for DB recording regardless of idle state
    if (this._onSample) this._onSample(ifName, rxMbps, txMbps, now);

    // Always push to ring buffer so history is available on next browser connect
    this._ensureHistory(ifName);
    this.hist.get(ifName).push({ ts: now, rx_mbps: rxMbps, tx_mbps: txMbps });

    // Collector health must advance even with no browser connected. Previously
    // this happened after the idle gate, so a healthy stream looked stale.
    this.state.lastTrafficTs  = now;
    this.state.lastTrafficErr = this._configError;
    this._loggedErr = false;

    if (this.io.engine.clientsCount === 0) return;

    const sample = { ifName, ts: now, rx_mbps: rxMbps, tx_mbps: txMbps, running, disabled };

    for (const { ifName: subIf, socket } of this.subscriptions.values()) {
      if (subIf === ifName) socket.emit('traffic:update', sample);
    }

    if (ifName === this.defaultIf) {
      this.io.emit('wan:status', this.lastWanStatus);
    }

  }

  start() {
    this._ensureHistory(this.defaultIf);
    this._startAllStream();
    this._startWatchdog();
  }

  stop() {
    this._stopAllStream();
    this._stopWatchdog();
    // Release socket listeners/references so a torn-down session can be GC'd.
    for (const { socket, onSelect, onDisconnect } of this._boundSockets.values()) {
      socket.off('traffic:select', onSelect);
      socket.off('disconnect', onDisconnect);
    }
    this._boundSockets.clear();
    this.subscriptions.clear();
  }
}

module.exports = TrafficCollector;
