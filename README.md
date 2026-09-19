**English** | [简体中文](./README_CN.md)

# Smart Load Balancer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that routes every client API key to the right upstream profile: quota that resets soonest gets spent first, each key is pinned to its own profile for warm prompt caches, and dry profiles are skipped automatically.

## The problem, in 30 seconds

Say you have 3 API keys (your laptop, your home server, a CI job) and 3 Codex accounts behind CLIProxyAPI:

```
WITHOUT this plugin                 WITH this plugin

key laptop ──┐                      key laptop ──▶ profile 1 (sticky)
key server ──┼──▶ profile 1         key server ──▶ profile 2 (sticky)
key CI     ──┘      (crowded!)      key CI     ──▶ profile 3 (sticky)
             profile 2, 3 sit idle
```

The built-in scheduler tends to stack concurrent keys onto the same profile. The other profiles sit idle, your effective concurrency is capped by one account's limits, and prompt caches get thrashed as different conversations fight over the same account.

This plugin pins each key to its own profile and keeps it there while it's healthy. If the profile runs dry or errors, the key moves on its own — no manual juggling.

## How it picks a profile

```
a request from key K arrives
        │
        ▼
does K already have a sticky profile?
  │ yes                    │ no
  ▼                        ▼
is it still healthy?    rank every profile — top one wins:
  │ yes     │ no          1. your quota_priorities (your VIP list)
  ▼         ▼             2. not claimed by another key right now
use it   fail over        3. quota known beats quota unknown
(cache stays warm)        4. resets soonest first — spend quota that renews soon
                          5. most-used first — fill it up before it resets
                          6. host priority, least load, last-used, ID order…
                        remember the winner as K's sticky profile
```

In plain terms: it spends quota that's about to renew before quota with a distant reset, never steals a profile another key is actively using while a free one exists, and keeps each key on the same profile so prompt caches stay warm.

## Where quota numbers come from

No hot polling. Quota arrives three ways:

```
every request ──▶ usage.handle ──▶ ledger: tokens spent, 429 blocks
                       │
                       └──▶ response headers: precise used% + reset time
                            (upstream quota events ride along for free)

you click "refresh" ──▶ quota.fetch ──▶ upstream ──▶ snapshot store
in the management UI      (only when you ask)

stale or cold account ──▶ marked for calibration ──▶ the next suitable
                              real request is borrowed for one shot;
                              its response headers refresh the snapshot.
                              No real traffic? No calibration — the plugin
                              never invents requests of its own.
```

A `429` from upstream is classified, not just retried: an explicit weekly/monthly exhaustion blocks the profile until the window ends; a quota failure that names no window backs off 5 hours; a clearly transient limit (e.g. too many concurrent requests) doesn't block at all.

## Install

### Option A — plugin store (recommended, no build needed)

Point CLIProxyAPI at this plugin's registry in `config.yaml`, restart, then install `smart-load-balancer` from the management UI's plugin store:

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/nitansde/smart-load-balancer/main/registry.json"
```

### Option B — manual build

1. `make build` → `smart-load-balancer.so` (`.dylib` on macOS, `.dll` on Windows)
2. Copy it into CLIProxyAPI's `plugins` directory.
3. Add to `config.yaml`:
   ```yaml
   plugins:
     enabled: true
     configs:
       smart-load-balancer:
         enabled: true
         sticky_ttl_seconds: 86400  # 24h default
   ```
4. Restart CLIProxyAPI.

## Configuration

The management UI shows three options; everything else keeps sane built-in defaults:

| Field | Default | What it does |
|---|---|---|
| `enabled` | `true` | Master switch. Off = the plugin steps aside and the host's default scheduler takes over. |
| `sticky_ttl_seconds` | `86400` (24h) | How long an idle key stays pinned to its profile. |
| `five_hour_boost` | `false` | 5h boost mode. When on, a 5h window at 100% with no countdown running borrows one small real request to kick off its countdown — same cherry-pick as the weekly mode, the plugin never sends requests of its own. |

<details>
<summary>Advanced knobs (only if you hand-edit config.yaml)</summary>

`providers`, `strategy` (accepted but ignored), `sticky`, `window_seconds` (120), `max_inflight_per_profile` (8), `quota_priorities`.

</details>

## Details

- **Client identity** is a SHA-256 hash of the inbound `Authorization` (or `X-Api-Key`) header. Raw key material is never stored or logged.
- **It never breaks requests.** If no candidate matches, or the plugin is off, it declines the pick and the host falls back to its default scheduler.
- **The `strategy` setting** is accepted for compatibility but ignored — the ranking above always decides.
- **Calibrating a stale or new account.** The plugin never sends probe requests. When an account's snapshot goes stale (a long-window reset passed) or an account is brand new, it is marked for calibration and the next suitable real request is borrowed for one shot — preferably from a key whose recent requests average small token counts (each key's last 100 requests). If no small-key traffic shows up within 6 hours, any key's request may be borrowed. The borrowed request never changes the key's sticky pinning, and a borrow that teaches nothing (non-quota failure) cools down for 10 minutes before retrying.
- **Ranking, precisely.** Fresh selection orders candidates: (1) your `quota_priorities`; (2) profiles claimed by other keys sort after unclaimed ones (only when every candidate is claimed does fewest-claimed win); (3) quota-known before unknown; (4) long-window (weekly/monthly) reset-soonest first — resets within 1 hour count as the same moment, unknown resets rank last; (5) fill-first within a tier: highest precise `used_percent`, else highest ledger token count, never-used last; (6) unknown-quota profiles follow host priority (higher first); (7) least recent load; (8) the key's last-used profile (soft hint only); (9) natural ID order, head wins.

## Development

```bash
make test    # unit tests
make vet     # go vet
make build   # build for this machine
make dist    # cross-compile all 6 platforms
make zip VERSION=0.1.0  # store-layout zips + checksums.txt
```

## License

MIT
