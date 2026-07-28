# Redis — credentials for Phoenix Database monitor

Redis does not have multi-user RBAC on every deployment. Prefer one of:

## A. Redis 6+ ACL (recommended)

As an admin client:

```redis
ACL SETUSER phoenix_monitor on >CHANGE_ME_STRONG_PASSWORD ~* &* +ping +info +echo
ACL SAVE
```

Minimal commands for Phoenix health checks: **`+ping`** is enough (`health_check` ping or select_1 both use PING).

Phoenix connection string:

```text
redis://phoenix_monitor:CHANGE_ME_STRONG_PASSWORD@HOST:6379/0
```

TLS:

```text
rediss://phoenix_monitor:CHANGE_ME_STRONG_PASSWORD@HOST:6380/0
```

## B. requirepass (single shared password)

In `redis.conf`:

```conf
requirepass CHANGE_ME_STRONG_PASSWORD
```

DSN:

```text
redis://:CHANGE_ME_STRONG_PASSWORD@HOST:6379/0
```

## C. Network-only (no password, private network)

Lab only. DSN:

```text
HOST:6379
```

Never expose Redis without auth on a public network.

## Verify

```bash
redis-cli -h HOST -p 6379 --user phoenix_monitor -a 'CHANGE_ME_STRONG_PASSWORD' PING
# or
redis-cli -h HOST -a 'CHANGE_ME_STRONG_PASSWORD' PING
```
