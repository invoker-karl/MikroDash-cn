'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const BetterSqlite3 = require('better-sqlite3');

const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'mikrodash-v10-upgrade-'));
process.env.DATA_DIR = dataDir;
const db = require('../src/db');

test('v0.7.8 user layouts survive the v11/v12 upgrade and nav becomes writable', () => {
  db.open();
  db.setLayout('upgrade-user', 'dashboard', { cards: ['traffic', 'talkers'] });
  db.setLayout('upgrade-user', 'topology', { zoom: 1.25 });
  db.close();

  // Recreate the exact pre-v12 constraint while retaining representative user
  // data. The remaining schema was produced by the real migrations above, so
  // this exercises an actual in-place upgrade rather than a hand-written mock.
  const file = path.join(dataDir, 'mikrodash.db');
  const legacy = new BetterSqlite3(file);
  legacy.pragma('foreign_keys = OFF');
  legacy.exec(`
    DROP INDEX IF EXISTS idx_audit_ts;
    DROP INDEX IF EXISTS idx_audit_router_ts;
    DROP INDEX IF EXISTS idx_audit_actor_ts;
    DROP TABLE IF EXISTS audit_events;
    CREATE TABLE user_layouts_v10 (
      user_id TEXT NOT NULL,
      kind TEXT NOT NULL CHECK (kind IN ('dashboard','topology')),
      data TEXT NOT NULL,
      updated_at INTEGER NOT NULL,
      PRIMARY KEY (user_id, kind)
    );
    INSERT INTO user_layouts_v10 SELECT * FROM user_layouts;
    DROP TABLE user_layouts;
    ALTER TABLE user_layouts_v10 RENAME TO user_layouts;
    DELETE FROM schema_version WHERE version >= 11;
  `);
  legacy.close();

  const upgraded = db.open();
  assert.deepEqual(db.getLayout('upgrade-user', 'dashboard'), { cards: ['traffic', 'talkers'] });
  assert.deepEqual(db.getLayout('upgrade-user', 'topology'), { zoom: 1.25 });
  db.setLayout('upgrade-user', 'nav', { grouped: true, open: ['network'] });
  assert.deepEqual(db.getLayout('upgrade-user', 'nav'), { grouped: true, open: ['network'] });
  assert.deepEqual(
    upgraded.prepare('SELECT version FROM schema_version WHERE version >= 11 ORDER BY version').all().map(r => r.version),
    [11, 12]
  );
  assert.ok(upgraded.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='audit_events'").get());
});

test.after(() => {
  db.close();
  fs.rmSync(dataDir, { recursive: true, force: true });
});
