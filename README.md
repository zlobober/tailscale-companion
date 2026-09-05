# Tailscale Companion

Run a second Tailscale tailnet alongside the official macOS app, with narrowly
controlled IPv4 routing and a background service that does not need a terminal.

**Unofficial, experimental, and macOS-only.** This project is not affiliated with
or endorsed by Tailscale. The menu-bar UI is planned, not implemented yet.

## The problem

The official macOS app can switch accounts, but switching disconnects one tailnet
to use another. A common requirement is keeping the app connected to a work
network while retaining access to a personal network.

Starting another stock daemon is not enough: both instances share macOS's routing
and DNS configuration. Tailscale can install aggregate routes for `100.64.0.0/10`
and `fd7a:115c:a1e0::/48`, which overlap between tailnets. DNS configuration can
also conflict.

Narrowing the secondary tailnet's **IP allocation pool** makes the desired traffic
split explicit, but does not constrain the routes its daemon installs. Likewise,
`--accept-routes=false` rejects advertised subnet routes, not ordinary peer routes.
The App Store app's `IPNExtension` cannot simply be run as a second standalone
`tailscaled`: it requires macOS's Network Extension/XPC context.

## How it works

```text
Official Tailscale app                         System LaunchDaemon
  primary/work tailnet                          Tailscale Companion (Go supervisor)
  app-managed tunnel, routes, DNS                  ├─ patched tailscaled → another utun
                                                  └─ monitor → one explicit IPv4 prefix
```

- **Patched daemon:** creates its tunnel, configures its own addresses, authenticates,
  and transports traffic. A private macOS-only `--private-no-routes` flag clears
  explicit OS route requests on every reconfiguration, preserving interface setup
  and teardown. It does not suppress implicit kernel routes for local addresses.
- **Go supervisor:** checks the authenticated tailnet, discovers the actual `utunN`,
  checks for route conflicts, and manages the configured secondary IPv4 prefix.
- **Daemon config:** disables DNS/subnet-route acceptance, exit-node use, route
  advertisements, and automatic updates that would replace the patched binary.
- **launchd:** runs root-owned copies of the supervisor, daemon, CLI, and configuration
  independently of Terminal/user login, and restarts the service after exit.

The official app remains responsible for the primary network. This is controlled
routing, **not separate network namespaces or a security boundary**.

## Status and scope

Implemented: command-line supervisor, route lifecycle, browser-login initiation,
root-owned installation/migration, and LaunchDaemon support.

Planned menu-bar UI, deliberately limited to:

1. Show secondary service/connection/routing status.
2. Start the secondary service.
3. Shut down the secondary service and remove its owned route.

The UI should use native macOS styling, feel familiar next to the official app,
and have a distinct personal icon, such as a house/network symbol. It must respect
administrator authorization. Account switching, device browsing, exit-node
selection, DNS settings, and other official-app features are out of scope.

## Requirements

- macOS; the current paths and daemon build have been exercised on Apple Silicon.
- Go 1.26 or newer for building.
- Homebrew's **formula**, not the GUI cask: `brew install --formula tailscale`.
  Development expects its CLI at `/opt/homebrew/opt/tailscale/bin/tailscale`.
- The patched daemon below, currently pinned to upstream **v1.102.3**.
- Administrator approval to create the tunnel, modify routes, and install the service.
- A dedicated IPv4 pool within Tailscale's CGNAT range for the secondary tailnet.
  Configure the central policy separately and renumber existing devices as needed.

Only one secondary instance is supported. Service/state/socket paths are fixed;
this is not a general-purpose multi-VPN manager.

## Build the daemon and companion

The expected development layout is `~/tailscale` for upstream and
`~/agent/tailscale-companion` for this repository:

```sh
mkdir -p ~/agent
git clone https://github.com/zlobober/tailscale-companion.git ~/agent/tailscale-companion
git clone --branch v1.102.3 https://github.com/tailscale/tailscale.git ~/tailscale

cd ~/tailscale
git switch -c companion/no-routes
git apply ~/agent/tailscale-companion/patches/tailscale-v1.102.3-no-routes.patch
go test ./cmd/tailscaled -run '^TestNoRoutesRouter' -count=1
mkdir -p dist
./build_dist.sh -o dist/tailscaled-private ./cmd/tailscaled

cd ~/agent/tailscale-companion
go test -race ./...
go build -o tailscale-companion .
```

If those directories already exist, inspect them rather than cloning or applying
the patch again. The daemon patch is included here for reproducibility; it is not
necessary to depend on an unpublished branch in a separate fork. Preserve its
upstream copyright notices and [BSD-3-Clause license](patches/TAILSCALE-LICENSE).

## Local configuration

```sh
cp companion.example.json companion.json
cp tailscaled.example.json tailscaled.json
```

Edit these **Git-ignored** local files:

- `companion.json`: set `expectedTailnet` to the exact tailnet name reported by
  Tailscale, and `ipv4Prefix` to its dedicated allocation prefix. The example
  `100.100.42.0/24` is illustrative, not a universally safe choice.
- `tailscaled.json`: choose the new device's `hostname`. Keep the DNS, routing,
  exit-node, and update safety preferences disabled.

The blank tailnet in the example is intentional: starting without an explicit
identity must fail rather than accidentally routing the primary network.
The configured prefix must be canonical, within `100.64.0.0/10`, and must not
include Tailscale's reserved ranges.

No credentials, keys, or machine state belong in this repository. Browser login
creates a separate device identity; the official app's state is never reused.

## First start and login

Preview without changing the system:

```sh
./tailscale-companion --dry-run
```

For an initial foreground run:

```sh
./tailscale-companion start
```

In another terminal:

```sh
./tailscale-companion login
./tailscale-companion cli status
```

Approve the secondary account in the browser. The route monitor waits for a running
node in the configured tailnet, discovers its tunnel, and installs the prefix.
Stop the foreground supervisor with Ctrl-C to clean up both route and daemon.

`cli ...` always uses the secondary socket; avoid bare `tailscale` commands when
it is unclear which daemon they target. If a standalone private daemon is already
running without a supervisor, `./tailscale-companion routes` manages only its routes;
Ctrl-C in that mode leaves the daemon running.

## Install as a background service

```sh
./tailscale-companion install-service
```

The installer requests administrator approval, stages/validates root-owned copies,
gracefully stops identified secondary processes, retains the same state location,
and bootstraps launchd. Expect a short interruption during migration or redeployment.
An initial install without prior login may report that routing is not ready; inspect
status and complete `./tailscale-companion login` rather than deleting state.

The following legacy service names are retained to preserve existing installations:

| Item | Location |
| --- | --- |
| LaunchDaemon label | `me.zlobober.tailscale-personal` |
| Definition | `/Library/LaunchDaemons/me.zlobober.tailscale-personal.plist` |
| Root-owned executables | `/usr/local/libexec/tailscale-personal/` |
| Root-owned configs | `/Library/Application Support/TailscalePersonal/` |
| Persistent identity/state | `/var/lib/tailscale-personal/` |
| Local API socket | `/var/run/tailscale-personal.socket` |
| Supervisor lock | `/var/run/tailscale-personal-wrapper.lock` |
| Service log | `/var/log/tailscale-personal.log` |

The running service does not read configuration or execute binaries from the writable
checkout/Homebrew directory. After changing sources or local configuration, rebuild
and run `install-service` to deploy; editing files alone does not change it.

### Operate the service

```sh
./tailscale-companion service-status
./tailscale-companion cli status
sudo tail -f /var/log/tailscale-personal.log

# Stop; use unload semantics, not kill (KeepAlive would restart it):
sudo launchctl bootout system/me.zlobober.tailscale-personal

# Start again:
sudo launchctl bootstrap system /Library/LaunchDaemons/me.zlobober.tailscale-personal.plist
```

`bootout` stops it until reloaded or rebooted. To disable startup across reboots,
also use `sudo launchctl disable system/me.zlobober.tailscale-personal`; re-enable
with `launchctl enable` before loading it again. Shutdown allows 45 seconds for
cleanup. The daemon stays in launchd's process group to avoid orphaning it when
the supervisor exits unexpectedly.

## Routing behavior and limitations

Every three seconds, the monitor verifies the tailnet and node IP, discovers the
unique active tunnel that owns that IP, and inspects the IPv4 routing table.

- It adds only the configured IPv4 prefix. A broader primary `/10` is left alone.
- Equal/more-specific routes within the prefix pointing elsewhere are conflicts.
- Existing unmanaged routes are not adopted, even on the same interface.
- Reconnects/interface changes remove the owned old route before adding the new one.
- Logout, wrong-tailnet selection, or orderly shutdown removes the owned route if
  it still points to the recorded interface. Foreign replacement routes are preserved.
- It never uses `route change` or deletes another route to force installation.

Use `route -n get <secondary-peer-ip>` and an ordinary `ping` or application to
verify system routing. A `tailscale ping` alone proves the daemon's data plane,
not which route a normal application will use.

Important limits:

- Route inspection/updates are not atomic; other VPNs can race them. Detection occurs
  on the next poll. This is not a fail-closed firewall: when the secondary route is
  absent, the primary aggregate may win again.
- macOS has no per-process route owner token. Replacements with identical prefix
  **and interface** are indistinguishable; do not concurrently manage this prefix.
- SIGKILL, crashes, and power loss cannot run cleanup. Inspect orphaned routes before
  removing them manually. A stale root-owned socket is removed only after confirming
  that no listener accepts connections; live sockets are never removed.
- Secondary IPv6 routes, MagicDNS, and LAN subnet routing are deliberately absent.
  Do not add the shared Tailscale IPv6 `/48` to the secondary tunnel.
- The daemon config uses upstream's experimental `alpha0` format and is unlocked
  for interactive login. Do not override safety preferences through later CLI calls.
- Logs are plain files; rotation is not configured yet.

## Source map and tests

- `main.go`: CLI commands, configuration validation, supervision.
- `routes.go`: identity checks, tunnel discovery, route lifecycle.
- `service.go`: root-owned deployment and LaunchDaemon definition.
- `settings.go`: local identity/prefix settings and validation.
- `*.example.json`: publishable templates, not active configuration.
- `patches/`: pinned upstream daemon patch and license.
- `*_test.go`: fake route/CLI operations, lifecycle and conflict tests, configuration,
  deployment paths, and process selection. No live route changes are made by tests.

Run `go test -race ./...` and `go vet ./...` before deploying. On upstream upgrades,
review the macOS router implementation again: the private flag assumes explicit
route operations are driven by the filtered configuration. Do not blindly replace
the daemon with a stock binary.
