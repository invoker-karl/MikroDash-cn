const path = require('path');

const PATCH_MARKERS = [
  'MIKRODASH_PATCHED_EMPTY_REPLY',
  'MIKRODASH_PATCHED_UNREGISTEREDTAG',
  'MIKRODASH_PATCHED_UTF8_ENCODING',
  'MIKRODASH_PATCHED_MULTI_BLOCK',
  'MIKRODASH_PATCHED_MULTI_BLOCK_V2',
];

function resolveDistPath(marker) {
  return marker.includes('EMPTY') || marker.includes('MULTI_BLOCK')
    ? 'Channel.js' : path.join('connector', 'Receiver.js');
}

function hasExactPatchMarker(src, marker) {
  const escaped = marker.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  // Markers may be a standalone comment or an inline end-of-line comment.
  // Token boundaries are deliberate: MULTI_BLOCK_V2 must never satisfy the
  // required MULTI_BLOCK marker merely because it shares that prefix.
  return new RegExp(`(?:^|[^A-Z0-9_])${escaped}(?![A-Z0-9_])`, 'm').test(src);
}

function verifyRouterOSPatchMarkers({
  patchMarkers = PATCH_MARKERS,
  distDir = path.join(__dirname, '..', '..', 'node_modules', 'node-routeros', 'dist'),
  readFileSync,
  log = console,
}) {
  for (const marker of patchMarkers) {
    const target = resolveDistPath(marker);
    const filePath = path.join(distDir, target);
    let src;

    try {
      src = readFileSync(filePath, 'utf8');
    } catch (error) {
      const msg = `[MikroDash] CRITICAL: Could not verify patch "${marker}" in ${target}: ${error.code || error.message}`;
      log.error(msg);
      throw new Error(msg);
    }

    if (!hasExactPatchMarker(src, marker)) {
      const msg = `[MikroDash] CRITICAL: node-routeros patch "${marker}" not found in ${target}`;
      log.error(msg);
      throw new Error(msg);
    }
  }
}

module.exports = {
  PATCH_MARKERS,
  resolveDistPath,
  hasExactPatchMarker,
  verifyRouterOSPatchMarkers,
};
