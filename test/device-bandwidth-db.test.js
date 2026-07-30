'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');

test('device bandwidth migration persists and aggregates daily totals', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'mikrodash-device-bw-'));
  process.env.DATA_DIR = tmpDir;
  delete require.cache[require.resolve('../src/db')];
  const db = require('../src/db');
  try {
    db.open();
    db.insertDeviceBandwidthSample('r1', '2026-07-30', '192.168.1.10', 10, 2, 1000);
    db.insertDeviceBandwidthSample('r1', '2026-07-30', '192.168.1.10', 5, 3, 2000);
    db.insertDeviceBandwidthSample('r1', '2026-07-30', '192.168.1.11', 7, 1, 3000);
    db.insertDeviceBandwidthSample('r1', '2026-07-29', '192.168.1.10', 99, 99, 4000);

    const totals = db.queryDeviceBandwidthTotals('r1', '2026-07-30');
    assert.deepEqual(totals, [
      { src_ip: '192.168.1.10', rx_mb: 15, tx_mb: 5 },
      { src_ip: '192.168.1.11', rx_mb: 7, tx_mb: 1 },
    ]);
  } finally {
    db.close();
    delete process.env.DATA_DIR;
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('daily traffic aggregation uses the requested timezone offset', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'mikrodash-traffic-tz-'));
  process.env.DATA_DIR = tmpDir;
  delete require.cache[require.resolve('../src/db')];
  const db = require('../src/db');
  try {
    db.open();
    const first = Date.UTC(2026, 6, 29, 17, 0, 0); // 2026-07-30 01:00 Asia/Shanghai
    const second = Date.UTC(2026, 6, 30, 15, 0, 0); // 2026-07-30 23:00 Asia/Shanghai
    db.insertTrafficSample('r1', 'bridge1', 10, 2, first);
    db.insertTrafficSample('r1', 'bridge1', 20, 4, second);

    const rows = db.queryTrafficSamplesAgg('r1', 'bridge1', first - 1, second + 1, 'day', 8 * 3600000);
    assert.equal(rows.length, 1);
    assert.equal(rows[0].ts, Date.UTC(2026, 6, 29, 16, 0, 0));
    assert.equal(rows[0].rx_mbps, 15);
    assert.equal(rows[0].tx_mbps, 3);
    assert.equal(rows[0].sample_count, 2);
  } finally {
    db.close();
    delete process.env.DATA_DIR;
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});
