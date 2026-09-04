import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import ts from 'typescript';

const webRoot = path.resolve(import.meta.dirname, '..');
const srcRoot = path.join(webRoot, 'src');
const normalise = (value) => String(value || '').replace(/&nbsp;/g, ' ').replace(/&mdash;/g, '—')
  .replace(/&rarr;/g, '→').replace(/&darr;/g, '↓').replace(/&uarr;/g, '↑')
  .replace(/&hellip;|&#8230;/g, '…').replace(/&times;/g, '×').replace(/&minus;/g, '−')
  .replace(/&larr;|&#8592;/g, '←').replace(/&#8593;/g, '↑').replace(/&#8594;/g, '→').replace(/&#8595;/g, '↓')
  .replace(/&#8212;/g, '—').replace(/&#183;/g, '·').replace(/&thinsp;/g, ' ')
  .replace(/&#9678;/g, '◎').replace(/&#9679;/g, '●').replace(/&#9889;/g, '⚡')
  .replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&amp;/g, '&')
  .replace(/\s+/g, ' ').trim();

const localePath = path.join(webRoot, 'public/locales/zh-CN.js');
const localeSource = fs.readFileSync(localePath, 'utf8');
const sandbox = { window: {} };
vm.runInNewContext(localeSource, sandbox);
const locale = sandbox.window.MikroDashLocales['zh-CN'];
const policy = JSON.parse(fs.readFileSync(path.join(webRoot, 'i18n/allowlist.json'), 'utf8'));
const allowed = new Set(Object.values(policy.exact).flat());
const allowedPatterns = policy.patterns.map((item) => new RegExp(item.regex));
const candidates = new Set();
const policyErrors = [];

// JavaScript silently keeps the last value of a duplicate locale key. That made
// two different translations look valid depending only on declaration order,
// while the runtime object and the old coverage audit had already lost the
// overwritten source location. Reject conflicting duplicates before evaluation.
const localeAst = ts.createSourceFile(localePath, localeSource, ts.ScriptTarget.Latest, true);
const localeDeclarations = new Map();
function localePropertyName(node) {
  if (ts.isStringLiteralLike(node) || ts.isIdentifier(node)) return node.text;
  return null;
}
function localeValue(node) {
  return ts.isStringLiteralLike(node) ? node.text : null;
}
function recordLocaleObject(node) {
  if (!ts.isObjectLiteralExpression(node)) return;
  for (const prop of node.properties) {
    if (!ts.isPropertyAssignment(prop)) continue;
    const key = localePropertyName(prop.name);
    const value = localeValue(prop.initializer);
    if (key === null || value === null) continue;
    const at = localeAst.getLineAndCharacterOfPosition(prop.getStart(localeAst)).line + 1;
    const entries = localeDeclarations.get(key) || [];
    entries.push({ value, line: at });
    localeDeclarations.set(key, entries);
  }
}
function collectLocaleObjects(node) {
  if (ts.isPropertyAssignment(node) && localePropertyName(node.name) === 'messages') {
    recordLocaleObject(node.initializer);
  }
  if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression) &&
      ts.isIdentifier(node.expression.expression) && node.expression.expression.text === 'Object' &&
      node.expression.name.text === 'assign' && node.arguments.some((arg) =>
        ts.isPropertyAccessExpression(arg) && arg.name.text === 'messages')) {
    for (const arg of node.arguments) recordLocaleObject(arg);
  }
  ts.forEachChild(node, collectLocaleObjects);
}
collectLocaleObjects(localeAst);
for (const [key, entries] of localeDeclarations) {
  const values = new Set(entries.map((entry) => entry.value));
  if (values.size > 1) {
    policyErrors.push(`conflicting duplicate locale key "${key}" at lines ${entries.map((entry) => entry.line).join(', ')}`);
  }
}

// The wordmark is deliberately split into two styled text nodes. Translating
// either half turns the product name into ordinary prose (for example,
// "Mikro短划线"), so protect both the markup and the locale catalogue.
const shellHTML = fs.readFileSync(path.join(srcRoot, 'ui/shell.html'), 'utf8');
if (!/<h1\s+id="topbarLogo"\s+data-i18n-skip>Mikro<span>Dash<\/span><\/h1>/.test(shellHTML)) {
  policyErrors.push('the MikroDash top-bar wordmark must be marked data-i18n-skip');
}
if (Object.prototype.hasOwnProperty.call(locale.messages, 'Dash') && locale.messages.Dash !== 'Dash') {
  policyErrors.push(`the protected brand fragment "Dash" must not translate to "${locale.messages.Dash}"`);
}

const settingsHTML = fs.readFileSync(path.join(srcRoot, 'ui/page-settings.html'), 'utf8');
if (!/<div class="theme-swatch-grid" id="themeSwatches" data-i18n-skip>/.test(settingsHTML)) {
  policyErrors.push('the theme-name grid must be marked data-i18n-skip so split product names stay intact');
}
if (Object.prototype.hasOwnProperty.call(locale.messages, 'Pro')) {
  policyErrors.push('the protected theme fragment "Pro" must not have a standalone translation');
}
for (const state of ['OpenSent', 'OpenConfirm']) {
  if (Object.prototype.hasOwnProperty.call(locale.messages, state)) {
    policyErrors.push(`the BGP FSM state "${state}" must remain a protocol identifier`);
  }
}
if (allowed.has('IPsec Peers') || locale.messages['IPsec Peers'] !== 'IPsec 对端') {
  policyErrors.push('"IPsec Peers" must translate as a UI phrase, not be hidden by the protocol allowlist');
}

const vpnSource = fs.readFileSync(path.join(srcRoot, 'pages/vpn.ts'), 'utf8');
if ((vpnSource.match(/<tr data-i18n-user-data>/g) || []).length < 2 ||
    !/vpn-tile-name-text" data-i18n-user-data/.test(vpnSource) ||
    !/vpn-tile-iface" data-i18n-user-data/.test(vpnSource) ||
    !/vpn-tile-ip" data-i18n-user-data/.test(vpnSource)) {
  policyErrors.push('VPN RouterOS/user values must be protected with data-i18n-user-data');
}

function add(value) {
  const text = normalise(value);
  if (!/[A-Za-z]{2}/.test(text) || text.length > 240 || allowed.has(text) || allowedPatterns.some((rx) => rx.test(text))) return;
  if (/^(?:https?:|wss?:|\/?[\w.-]+\/|[.#][\w-]+|--[\w-]+|rgba?\(|var\(|[\w-]+:)/i.test(text)) return;
  if (/^[\w.-]+@[\w.-]+$/.test(text) || /^\{\{.*\}\}$/.test(text)) return;
  if (/\b(?:class|id|style|max|min|step|value)="/.test(text) || /^[",.?]\s/.test(text)) return;
  if (/^Remove the (?:queue|scheduled report) "$/.test(text) || /^"\? Traffic it was limiting/.test(text) || /^Mbps\)$/.test(text)) return;
  candidates.add(text);
}

function addHTML(source) {
  let html = source.replace(/<!--[\s\S]*?-->/g, ' ')
    .replace(/<(script|style|pre|textarea)\b[\s\S]*?<\/\1>/gi, ' >< ')
    .replace(/<[^>]+data-i18n-(?:skip|user-data)[^>]*>[\s\S]*?<\/[^>]+>/gi, ' >< ');
  for (const match of html.matchAll(/>([^<]+)</g)) add(match[1]);
  for (const match of html.matchAll(/\b(?:placeholder|title|alt|aria-label|aria-description|aria-valuetext)=(?:"([^"]*)"|'([^']*)')/gi)) add(match[1] ?? match[2]);
}

function walkFiles(dir, suffix, visit) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) walkFiles(file, suffix, visit);
    else if (file.endsWith(suffix)) visit(file);
  }
}

walkFiles(path.join(srcRoot, 'ui'), '.html', (file) => addHTML(fs.readFileSync(file, 'utf8')));

function collect(node) {
  if (!node) return;
  if (ts.isStringLiteralLike(node)) {
    if (node.text.includes('<')) addHTML(node.text);
    else add(node.text);
    return;
  }
  if (ts.isTemplateExpression(node)) {
    addHTML(node.head.text);
    for (const span of node.templateSpans) addHTML(span.literal.text);
    return;
  }
  if (ts.isConditionalExpression(node)) {
    collect(node.whenTrue); collect(node.whenFalse); return;
  }
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.PlusToken) {
    collect(node.left); collect(node.right); return;
  }
  if (ts.isArrayLiteralExpression(node)) for (const item of node.elements) collect(item);
  if (ts.isObjectLiteralExpression(node)) for (const prop of node.properties) {
    if (ts.isPropertyAssignment(prop)) collect(prop.initializer);
  }
}

function containsNativeTranslation(node) {
  let found = false;
  const visit = (child) => {
    if (found || !child) return;
    if (ts.isCallExpression(child) && ts.isIdentifier(child.expression) && child.expression.text === 'tr') {
      found = true;
      return;
    }
    ts.forEachChild(child, visit);
  };
  visit(node);
  return found;
}

walkFiles(srcRoot, '.ts', (file) => {
  const sourceText = fs.readFileSync(file, 'utf8');
  const source = ts.createSourceFile(file, sourceText, ts.ScriptTarget.Latest, true);
  const visit = (node) => {
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken &&
        ts.isPropertyAccessExpression(node.left) && ['textContent', 'innerHTML'].includes(node.left.name.text)) collect(node.right);
    if (ts.isCallExpression(node) && ts.isPropertyAccessExpression(node.expression)) {
      const name = node.expression.name.text;
      if (name === 'insertAdjacentHTML') collect(node.arguments[1]);
      if (name === 'setAttribute' && ts.isStringLiteral(node.arguments[0]) &&
          ['placeholder', 'title', 'alt', 'aria-label', 'aria-description', 'aria-valuetext'].includes(node.arguments[0].text)) collect(node.arguments[1]);
      if (['confirm', 'prompt', 'alert'].includes(name)) {
        collect(node.arguments[0]);
        if (!containsNativeTranslation(node.arguments[0])) {
          const at = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
          policyErrors.push(`${path.relative(webRoot, file)}:${at} ${name}() text must pass through tr()`);
        }
      }
    }
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) &&
        ['confirm', 'prompt', 'alert'].includes(node.expression.text)) {
      collect(node.arguments[0]);
      if (!containsNativeTranslation(node.arguments[0])) {
        const at = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
        policyErrors.push(`${path.relative(webRoot, file)}:${at} ${node.expression.text}() text must pass through tr()`);
      }
    }
    if (ts.isPropertyAssignment(node) && (ts.isIdentifier(node.name) || ts.isStringLiteral(node.name)) &&
        ['label', 'title', 'empty', 'description', 'placeholder'].includes(node.name.text)) collect(node.initializer);
    ts.forEachChild(node, visit);
  };
  visit(source);
});

const missing = [...candidates].filter((text) => !Object.prototype.hasOwnProperty.call(locale.messages, text)).sort();
if (missing.length || policyErrors.length) {
  if (missing.length) console.error(`i18n audit: ${missing.length} English UI strings are missing from zh-CN.js:`);
  for (const text of missing) console.error('  - ' + text);
  for (const error of policyErrors) console.error('i18n policy: ' + error);
  process.exitCode = 1;
} else {
  console.log(`i18n audit passed: ${candidates.size} UI strings have Chinese translations.`);
}
