'use strict';

// Apply a live /interface/print snapshot to exactly one router session. Keeping
// this operation small and independently testable makes the room/cache boundary
// explicit: callers must supply the session's own scoped routerIo instance.
function applySessionInterfaceMetadata(session, routerIo, interfaces) {
  if (!session || session._destroyed) return false;
  const snapshot = Array.isArray(interfaces) ? interfaces : [];
  session._interfacesRevision = (session._interfacesRevision || 0) + 1;
  session.cachedInterfaces = snapshot;
  session._ifacesFetch = Promise.resolve(snapshot);
  session.traffic.setAvailableInterfaces(snapshot);
  routerIo.emit('interfaces:list', {
    ok: true, defaultIf: session.DEFAULT_IF, interfaces: snapshot,
  });
  return true;
}

module.exports = { applySessionInterfaceMetadata };
