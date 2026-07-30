'use strict';
const db = require('./db');

// ── Traffic aggregator ────────────────────────────────────────────────────────
// Accumulates per-interface rx/tx samples in memory and flushes 1-minute averages
// to the DB once per minute. Keyed by routerId + interface.

const _trafficBuckets   = new Map(); // `${routerId}:${iface}` → { minuteTs, sumRx, sumTx, count }
// Parallel bandwidth accumulator — sums actual MB transferred per minute.
// Each onSample call represents 1 second of data, so MB = Mbps / 8 per call.
const _bandwidthBuckets = new Map(); // `${routerId}:${iface}` → { minuteTs, sumRxMb, sumTxMb }
const _deviceBandwidthBuckets = new Map(); // `${routerId}\0${srcIp}` → minute/day byte totals
// Ping accumulator — 1-minute averages keyed by routerId + target. Avoids a
// synchronous INSERT per ping sample (~1/s) on the Socket.IO emit hot path and
// cuts ping_samples row growth ~60×, matching the traffic/bandwidth bucketing.
const _pingBuckets      = new Map(); // `${routerId}:${target}` → { minuteTs, sumRtt, rttCount, sumLoss, count }

function _minuteFloor(ts) {
  return Math.floor(ts / 60000) * 60000;
}

// Bucket keys are `${routerId}:${name}` where name (interface or ping target)
// may itself contain colons (IPv6). Split on the FIRST colon only.
function _splitKey(key) {
  const i = key.indexOf(':');
  return { rid: key.slice(0, i), name: key.slice(i + 1) };
}

function recordTraffic(routerId, ifName, rxMbps, txMbps, ts) {
  if (!routerId || !ifName) return;
  const bucketTs = _minuteFloor(ts || Date.now());
  const key      = routerId + ':' + ifName;

  // ── throughput average (traffic_samples) ─────────────────────────
  const tBucket = _trafficBuckets.get(key);
  if (tBucket && tBucket.minuteTs === bucketTs) {
    tBucket.sumRx += rxMbps;
    tBucket.sumTx += txMbps;
    tBucket.count += 1;
  } else {
    if (tBucket && tBucket.count > 0) {
      db.insertTrafficSample(routerId, ifName,
        tBucket.sumRx / tBucket.count,
        tBucket.sumTx / tBucket.count,
        tBucket.minuteTs + 30000);
    }
    _trafficBuckets.set(key, { minuteTs: bucketTs, sumRx: rxMbps, sumTx: txMbps, count: 1 });
  }

  // ── bandwidth accumulation (bandwidth_usage) ──────────────────────
  // Each call = 1 second of data; Mbps / 8 = MB per second
  const rxMb   = rxMbps / 8;
  const txMb   = txMbps / 8;
  const bBucket = _bandwidthBuckets.get(key);
  if (bBucket && bBucket.minuteTs === bucketTs) {
    bBucket.sumRxMb += rxMb;
    bBucket.sumTxMb += txMb;
  } else {
    if (bBucket && bBucket.sumRxMb + bBucket.sumTxMb > 0) {
      db.insertBandwidthSample(routerId, ifName,
        bBucket.sumRxMb, bBucket.sumTxMb,
        bBucket.minuteTs + 30000);
    }
    _bandwidthBuckets.set(key, { minuteTs: bucketTs, sumRxMb: rxMb, sumTxMb: txMb });
  }
}

function _flushDeviceBucket(bucket) {
  if (!bucket || bucket.rxMb + bucket.txMb <= 0) return;
  db.insertDeviceBandwidthSample(
    bucket.routerId, bucket.dayKey, bucket.srcIp,
    bucket.rxMb, bucket.txMb, bucket.minuteTs + 30000
  );
}

function recordDeviceBandwidth(routerId, dayKey, srcIp, rxBytes, txBytes, ts) {
  if (!routerId || !dayKey || !srcIp) return;
  const rxMb = Math.max(0, Number(rxBytes) || 0) / 1_000_000;
  const txMb = Math.max(0, Number(txBytes) || 0) / 1_000_000;
  if (rxMb + txMb <= 0) return;
  const minuteTs = _minuteFloor(ts || Date.now());
  const key = routerId + '\0' + srcIp;
  const bucket = _deviceBandwidthBuckets.get(key);
  if (bucket && bucket.minuteTs === minuteTs && bucket.dayKey === dayKey) {
    bucket.rxMb += rxMb;
    bucket.txMb += txMb;
    return;
  }
  _flushDeviceBucket(bucket);
  _deviceBandwidthBuckets.set(key, { routerId, dayKey, srcIp, minuteTs, rxMb, txMb });
}

// Flush all open buckets — call on session teardown to avoid data loss
function flushTraffic(routerId) {
  for (const [key, bucket] of _trafficBuckets) {
    if (bucket.count === 0) continue;
    const { rid, name } = _splitKey(key);
    if (routerId && rid !== routerId) continue;
    db.insertTrafficSample(rid, name,
      bucket.sumRx / bucket.count,
      bucket.sumTx / bucket.count,
      bucket.minuteTs + 30000);
    _trafficBuckets.delete(key);
  }
  for (const [key, bBucket] of _bandwidthBuckets) {
    if (bBucket.sumRxMb + bBucket.sumTxMb === 0) continue;
    const { rid, name } = _splitKey(key);
    if (routerId && rid !== routerId) continue;
    db.insertBandwidthSample(rid, name,
      bBucket.sumRxMb, bBucket.sumTxMb,
      bBucket.minuteTs + 30000);
    _bandwidthBuckets.delete(key);
  }
  for (const [key, p] of _pingBuckets) {
    if (p.count === 0) continue;
    const { rid, name } = _splitKey(key);
    if (routerId && rid !== routerId) continue;
    db.insertPingSample(rid, name,
      p.rttCount > 0 ? p.sumRtt / p.rttCount : null,
      p.sumLoss / p.count,
      p.minuteTs + 30000);
    _pingBuckets.delete(key);
  }
  for (const [key, bucket] of _deviceBandwidthBuckets) {
    if (routerId && bucket.routerId !== routerId) continue;
    _flushDeviceBucket(bucket);
    _deviceBandwidthBuckets.delete(key);
  }
}

// ── Ping writer ───────────────────────────────────────────────────────────────
// Accumulates 1-minute averages (avg rtt over non-null samples, avg loss) and
// flushes the previous minute's bucket on rollover, mirroring recordTraffic.

function recordPing(routerId, target, rttMs, lossPct, ts) {
  if (!routerId) return;
  const bucketTs = _minuteFloor(ts || Date.now());
  const key      = routerId + ':' + (target || '');
  const b = _pingBuckets.get(key);
  if (b && b.minuteTs === bucketTs) {
    if (rttMs != null) { b.sumRtt += rttMs; b.rttCount += 1; }
    b.sumLoss += lossPct;
    b.count   += 1;
  } else {
    if (b && b.count > 0) {
      db.insertPingSample(routerId, target,
        b.rttCount > 0 ? b.sumRtt / b.rttCount : null,
        b.sumLoss / b.count,
        b.minuteTs + 30000);
    }
    _pingBuckets.set(key, {
      minuteTs: bucketTs,
      sumRtt:   rttMs != null ? rttMs : 0,
      rttCount: rttMs != null ? 1 : 0,
      sumLoss:  lossPct,
      count:    1,
    });
  }
}

// ── Connectivity writer ───────────────────────────────────────────────────────

function recordConnectivity(routerId, connected) {
  if (!routerId) return;
  db.insertConnectivityEvent(routerId, connected);
}

module.exports = { recordTraffic, recordDeviceBandwidth, flushTraffic, recordPing, recordConnectivity };
