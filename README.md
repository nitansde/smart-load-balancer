# Smart Load Balancer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that replaces the default auth-scheduling policy with a load-aware one.

## The problem

With several client API keys and several upstream auth profiles (for example 8 Codex profiles), the built-in scheduler tends to place concurrent requests from different keys onto the **same** profile. That wastes the other profiles, lowers effective concurrency, and thrashes prompt caches.

## What it does

When enabled, every auth pick goes through this plugin instead of the default scheduler:

- **Least-connections spreading** — each request is routed to the eligible profile with the fewest recent picks inside a sliding window, so simultaneous requests from different keys land on different profiles.
- **Per-key stickiness** — a client API key is pinned to one profile while that profile stays healthy, keeping its prompt cache warm. If the pinned profile gets saturated, the key transparently spills over to the least-loaded one.
- **Deterministic tie-breaking** — when several profiles are equally idle, different keys still pick different profiles (the tie-break is a hash of the key identity and the profile id), while the same key stays stable.

Client identity is derived as a SHA-256 hash of the inbound `Authorization` (or `X-Api-Key`) header. Raw key material is never stored or logged.

## Install

### From the plugin store (recommended)

Once published to the official store, install from the CLIProxyAPI management UI or CLI by searching for `smart-load-balancer`.

### Manual

1. Build the shared library for your platform:
   ```bash
   make build
   # produces smart-load-balancer.so (.dylib on macOS, .dll on Windows)
   ```
2. Copy it into your CLIProxyAPI `plugins` directory.
3. Add to `config.yaml`:
   ```yaml
   plugins:
     enabled: true
     configs:
       smart-load-balancer:
         enabled: true
         priority: 1
         providers: ["codex"]      # optional: only balance these providers; empty = all
         strategy: least-connections # or: round-robin
         sticky: true
         sticky_ttl_seconds: 1800
         window_seconds: 120
         max_inflight_per_profile: 8
   ```
4. Restart CLIProxyAPI.

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `providers` | array | `[]` | Only balance across these provider keys (e.g. `codex`). Empty means every provider offered by the host. |
| `strategy` | enum | `least-connections` | `least-connections` spreads load; `round-robin` cycles through profiles in stable id order. |
| `sticky` | bool | `true` | Pin each client API key to one profile while it stays healthy (prompt-cache affinity). |
| `sticky_ttl_seconds` | int | `1800` | How long an idle sticky assignment is kept. |
| `window_seconds` | int | `120` | Sliding window used to estimate recent load per profile. |
| `max_inflight_per_profile` | int | `8` | Recent-pick threshold above which a sticky assignment spills over to the least-loaded profile. |

If no candidate matches `providers`, or no candidates are offered at all, the plugin declines the pick and the host falls back to its default scheduling — requests are never broken by this plugin.

## How it works

The plugin implements the `scheduler.pick` capability. On each pick the host offers the eligible auth candidates plus the inbound request headers; the plugin returns the chosen auth id. Load is estimated from the plugin's own recent picks inside `window_seconds` (self-healing: no cross-request bookkeeping can leak, and a restart simply starts with a clean slate).

## Development

```bash
make test    # unit tests for the balancing core
make vet     # go vet
make build   # build the plugin for the host platform
make dist    # cross-compile all 6 platform artifacts
make zip VERSION=0.1.0  # package store-layout zips + checksums.txt
```

An end-to-end check drives the compiled `.so` through the real C ABI (`plugin.register` → `scheduler.pick` → `plugin.reconfigure`) — see the test script used during development.

## Publishing to the official store

1. Replace `nitansde` in `go.mod`, `main.go`, and `plugin-registry-entry.json` with the real repo owner, then push to GitHub.
2. Tag a release: `git tag v0.1.0 && git push origin v0.1.0`. The `release` workflow builds the six platform zips plus `checksums.txt` and attaches them to the GitHub release.
3. Open a PR to `router-for-me/CLIProxyAPI-Plugins-Store` adding `plugin-registry-entry.json`'s object to `registry.json`.

## License

MIT
