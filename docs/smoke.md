# Smoke Test Checklist

Run this after every merge that should go into the `R5S` container.

## Pre-checks

- `docker compose ps` shows the container as `running`
- `curl -fsS http://127.0.0.1:3081/healthz` returns `200`
- `docker compose logs --tail=100 mikrodash` shows a healthy RouterOS connection

## UI checks

- Open the dashboard and confirm Basic Auth works.
- Dashboard page renders without blank cards.
- The traffic picker includes every non-disabled interface, labels link-down
  interfaces, and keeps a selected down interface instead of silently changing it.
- DHCP page loads leases.
- VPN page loads WireGuard peers or renders a clean empty state.
- Logs page updates live.

## RouterOS data checks

- WAN/default interface matches `DEFAULT_IF`.
- `interfaceStatus` shows non-zero throughput on an active interface.
- Connection counts update over time.
- If the router has wireless or WireGuard disabled, the UI still renders without errors.

## Resilience checks

- Restart the container and confirm the dashboard recovers:

```bash
docker compose restart mikrodash
curl -fsS http://127.0.0.1:3081/healthz
```

- If safe in your lab, briefly interrupt API access and confirm the app reconnects once access returns.

## Release gate

Treat the build as releasable only when:

- smoke checklist passes
- `npm test` is green on the same commit
- no unexpected reconnect loops appear in container logs
