'use strict';

const UNSUPPORTED_RE = /unknown command|no such command|no such item/i;
const PERMISSION_RE = /not permitted|not allowed|permission denied|not enough privileges|cannot run/i;

// node-routeros RStream emits arrays from prepareDebounceEmptyData() whenever
// no real packet arrived during its timer window. That is an idle signal, not a
// RouterOS snapshot and never proof that the underlying table is empty.
function classifyRStreamPacket(packet) {
  if (Array.isArray(packet)) return { kind: 'idle' };
  if (packet && typeof packet === 'object') return { kind: 'data', row: packet };
  return { kind: 'invalid' };
}

function classifySnapshotError(error) {
  const message = String(error && error.message ? error.message : error || '');
  if (PERMISSION_RE.test(message)) return { kind: 'permission', message };
  if (UNSUPPORTED_RE.test(message)) return { kind: 'unsupported', message };
  return { kind: 'transient', message };
}

/**
 * Coalesces synthetic-idle signals into an authoritative one-shot /print.
 * A result is applied only if the owner has not stopped/reconnected and no real
 * stream row arrived since the probe began. Promise cancellation is not needed:
 * generation + realRowVersion make every late completion harmless.
 */
class AuthoritativeSnapshotProbe {
  constructor({ read, apply, onError, cooldownMs = 1000 }) {
    this.read = read;
    this.apply = apply;
    this.onError = onError || (() => {});
    this.cooldownMs = Math.max(0, Number(cooldownMs) || 0);
    this.generation = 0;
    this.realRowVersion = 0;
    this.inFlight = null;
    this._timer = null;
    this._pending = false;
    this._lastStart = 0;
  }

  noteRealRow() {
    this.realRowVersion += 1;
    // An idle that was waiting for the cooldown predates this real packet. Do
    // not start a fresh probe afterward and let it erase the newer stream row.
    this._pending = false;
    if (this._timer) clearTimeout(this._timer);
    this._timer = null;
  }

  onIdle() {
    this._pending = true;
    if (this.inFlight || this._timer) return;
    const delay = Math.max(0, this.cooldownMs - (Date.now() - this._lastStart));
    if (delay > 0) {
      this._timer = setTimeout(() => {
        this._timer = null;
        this._start();
      }, delay);
      return;
    }
    this._start();
  }

  _start() {
    if (this.inFlight || !this._pending) return;
    this._pending = false;
    this._lastStart = Date.now();
    const generation = this.generation;
    const realRowVersion = this.realRowVersion;
    const request = Promise.resolve().then(() => this.read());
    this.inFlight = request;
    request.then((rows) => {
      if (generation !== this.generation || realRowVersion !== this.realRowVersion) return;
      if (!Array.isArray(rows)) throw new TypeError('Authoritative snapshot must be an array');
      this.apply(rows);
    }).catch((error) => {
      if (generation === this.generation && realRowVersion === this.realRowVersion) {
        this.onError(error, classifySnapshotError(error));
      }
    }).finally(() => {
      if (this.inFlight === request) this.inFlight = null;
      if (generation === this.generation && this._pending) this.onIdle();
    });
  }

  invalidate() {
    this.generation += 1;
    this.realRowVersion = 0;
    this.inFlight = null;
    this._pending = false;
    if (this._timer) clearTimeout(this._timer);
    this._timer = null;
    this._lastStart = 0;
  }
}

module.exports = {
  AuthoritativeSnapshotProbe,
  classifyRStreamPacket,
  classifySnapshotError,
};
