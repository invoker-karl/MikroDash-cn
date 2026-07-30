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
