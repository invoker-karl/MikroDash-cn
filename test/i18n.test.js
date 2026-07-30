'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = path.resolve(__dirname, '..');

function loadLocale(name) {
  const window = {};
  const source = fs.readFileSync(path.join(root, 'public', 'locales', name + '.js'), 'utf8');
  vm.runInNewContext(source, { window });
  return window.MikroDashLocales[name];
}

test('Simplified Chinese locale contains the main navigation and settings vocabulary', () => {
  const locale = loadLocale('zh-CN');
  assert.equal(locale.name, '简体中文');
  assert.equal(locale.messages.Dashboard, '仪表盘');
  assert.equal(locale.messages.Interfaces, '接口');
  assert.equal(locale.messages.Settings, '设置');
  assert.equal(locale.messages['Interface Language'], '界面语言');
  assert.ok(Object.keys(locale.messages).length >= 300);
  assert.ok(locale.patterns.length >= 10);
});

test('English locale remains the source-language fallback', () => {
  const locale = loadLocale('en-US');
  assert.equal(locale.name, 'English');
  assert.deepEqual(Object.keys(locale.messages), []);
});

test('application and login pages load locales before their page scripts', () => {
  const index = fs.readFileSync(path.join(root, 'public', 'index.html'), 'utf8');
  const login = fs.readFileSync(path.join(root, 'public', 'login.html'), 'utf8');

  assert.ok(index.indexOf('/locales/zh-CN.js') < index.indexOf('/i18n.js'));
  assert.ok(index.indexOf('/i18n.js') < index.indexOf('/app.js'));
  assert.ok(login.indexOf('/locales/zh-CN.js') < login.indexOf('/i18n.js'));
  assert.ok(login.indexOf('/i18n.js') < login.indexOf('/login.js'));
  assert.match(index, /data-language-select/);
  assert.match(login, /data-language-select/);
});

test('authentication middleware allows translation assets before sign-in', () => {
  const server = fs.readFileSync(path.join(root, 'src', 'index.js'), 'utf8');
  assert.match(server, /'\/i18n\.js'/);
  assert.match(server, /req\.path\.startsWith\('\/locales\/'\)/);
});
