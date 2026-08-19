'use strict';

const RELEASE_TAG = /^v(\d+)\.(\d+)\.(\d+)-cn\.(\d+)$/;

function parseReleaseTag(tag) {
  const match = RELEASE_TAG.exec(String(tag || ''));
  return match ? match.slice(1).map(Number) : null;
}

function compareReleaseTags(left, right) {
  const a = parseReleaseTag(left);
  const b = parseReleaseTag(right);
  if (!a || !b) throw new Error('Invalid Chinese release tag');
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

function mayPromote(current, tags) {
  if (!parseReleaseTag(current)) return false;
  const valid = tags.filter(parseReleaseTag);
  if (!valid.length) return false;
  valid.sort(compareReleaseTags);
  return valid.at(-1) === current;
}

if (require.main === module) {
  const [current, ...tags] = process.argv.slice(2);
  if (!mayPromote(current, tags)) {
    console.error(`Refusing stale promotion: current=${current || ''}`);
    process.exitCode = 1;
  }
}

module.exports = { parseReleaseTag, compareReleaseTags, mayPromote };
