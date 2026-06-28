# ninemeterorbit

## Setup

```sh
mise install                       # Go toolchain
cp .env.local.example .env.local   # then edit (gitignored)
```

`.env.local`:

```
NTFY_TOPIC=your-ntfy-topic     # optional; omit to just print
DEPLOY_HOST=user@host          # for `mise run deploy`
DEPLOY_PORT=22
```

Put the product URLs (one per line, each with `?variant=<id>`) in `urls.txt`.

## Use

```sh
mise run run       # scrape + print (and notify if NTFY_TOPIC is set)
mise run deploy    # cross-compile + scp binary & urls.txt to the VPS
mise run build     # local binary -> bin/
```

Scheduled 3×/day via cron on the VPS:

```cron
0 6,12,18 * * * cd /root/ninemeterorbit && NTFY_TOPIC=… ./ninemeterorbit-linux >> scrape.log 2>&1
```

## Notifications

https://ntfy.sh/

