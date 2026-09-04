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

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(path.join(webRoot, 'public/locales/zh-CN.js'), 'utf8'), sandbox);
const locale = sandbox.window.MikroDashLocales['zh-CN'];
const policy = JSON.parse(fs.readFileSync(path.join(webRoot, 'i18n/allowlist.json'), 'utf8'));
const allowed = new Set(Object.values(policy.exact).flat());
const allowedPatterns = policy.patterns.map((item) => new RegExp(item.regex));
const candidates = new Set();

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
      if (['confirm', 'prompt', 'alert'].includes(name)) collect(node.arguments[0]);
    }
    if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) &&
        ['confirm', 'prompt', 'alert'].includes(node.expression.text)) collect(node.arguments[0]);
    if (ts.isPropertyAssignment(node) && (ts.isIdentifier(node.name) || ts.isStringLiteral(node.name)) &&
        ['label', 'title', 'empty', 'description', 'placeholder'].includes(node.name.text)) collect(node.initializer);
    ts.forEachChild(node, visit);
  };
  visit(source);
});

const missing = [...candidates].filter((text) => !Object.prototype.hasOwnProperty.call(locale.messages, text)).sort();
if (missing.length) {
  console.error(`i18n audit: ${missing.length} English UI strings are missing from zh-CN.js:`);
  for (const text of missing) console.error('  - ' + text);
  process.exitCode = 1;
} else {
  console.log(`i18n audit passed: ${candidates.size} UI strings have Chinese translations.`);
}
