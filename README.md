# Tailscale Companion

Run a second Tailscale tailnet alongside the official macOS app, with narrowly
controlled IPv4 routing and a background service that does not need a terminal.

**Unofficial, experimental, and macOS-only.** This project is not affiliated with
or endorsed by Tailscale. Includes a native macOS menu-bar app.

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
                                                  └─ monitor → personal prefix + DNS service /32
```

- **Patched daemon:** creates its tunnel, configures its own addresses, authenticates,
  and transports traffic. A private macOS-only `--private-no-routes` flag clears
  explicit OS route requests on every reconfiguration, preserving interface setup
  and teardown. Its built-in DNS service uses companion-only `100.100.100.101`
  instead of the standard `100.100.100.100`. The patch does not suppress implicit
  kernel routes for local addresses.
- **Go supervisor:** checks the authenticated tailnet, discovers the actual `utunN`,
  checks for conflicts, manages the configured secondary IPv4 prefix and DNS service
  `/32`, and installs split DNS for only the secondary MagicDNS suffix.
- **Daemon config:** disables automatic DNS/subnet-route acceptance, exit-node use, route
  advertisements, and automatic updates that would replace the patched binary.
- **launchd:** runs root-owned copies of the supervisor, daemon, CLI, and configuration
  independently of Terminal/user login, and restarts the service after exit.

The official app remains responsible for the primary network. This is controlled
routing, **not separate network namespaces or a security boundary**.

## Status and scope

Implemented: command-line supervisor, route lifecycle, browser-login initiation,
root-owned installation/migration, LaunchDaemon support, and a native menu-bar app.

The menu-bar UI is deliberately limited to:

1. Show secondary service/connection/routing status.
2. Start the secondary service.
3. Shut down the secondary service and remove its owned route.

The UI uses native AppKit menus and a monochrome house icon with a state badge,
clearly distinct from the official app's dots. Start/Shut Down requests use macOS's
administrator prompt and target only the companion LaunchDaemon. Account switching,
device browsing, exit-node selection, DNS settings, and other official-app features
are out of scope.

## Requirements

- macOS; the current paths and daemon build have been exercised on Apple Silicon.
- Go 1.26 or newer for building.
- For the menu-bar app: macOS 14+, Swift 6+, and current Xcode Command Line Tools
  (or full Xcode). No external Swift package dependencies are needed.
- Homebrew's **formula**, not the GUI cask: `brew install --formula tailscale`.
  Development expects its CLI at `/opt/homebrew/opt/tailscale/bin/tailscale`.
- The patched daemon below, currently pinned to upstream **v1.102.3**.
- Administrator approval to create the tunnel, modify routes, and install the service.
- A dedicated IPv4 pool within Tailscale's CGNAT range and MagicDNS enabled for
  the secondary tailnet. Configure the central policy separately and renumber existing
  devices as needed.

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
go test ./cmd/tailscaled ./net/tsaddr -run 'Test(NoRoutesRouter|TailscaleServiceIP)' -count=1
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
- `tailscaled.json`: choose the new device's `hostname`. Keep automatic DNS and
  routing, exit-node, and update safety preferences disabled. The supervisor installs
  only the verified companion MagicDNS suffix itself.

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

## Menu-bar app

First install/update the service so it provides the read-only `ui-status` endpoint:

```sh
go build -o tailscale-companion .
./tailscale-companion install-service
```

Then build, test, and install the app **as your normal user**, without sudo:

```sh
go run ./cmd/menubar -test
go run ./cmd/menubar -install
```

The Go packaging helper builds the native Swift/AppKit app, ad-hoc signs it for
local use, installs `~/Applications/Tailscale Companion.app`, and loads a user
LaunchAgent to show the icon now and at future GUI logins. It does not change the
system daemon's state. Full Xcode is not required; the test helper supplies the
extra Swift Testing framework paths needed by Command Line Tools installations.

Click the **house icon** in the menu bar:

- **Status:** service/connection state, personal IP when known, and routing readiness.
- **Start Personal Tailscale:** authorize loading the existing system service.
- **Shut Down Personal Tailscale:** authorize unloading it, allowing route cleanup.

The icon uses a filled house/dot when connected with routing ready, an outline
when stopped, and an attention badge during transitions or failures. Appearance
follows macOS light/dark mode. Status polls every five seconds and refreshes when
the menu opens. Controls are disabled while an action is pending; cancelling an
authorization prompt is not treated as an error.

The app is unprivileged. Status comes from the root-owned installed wrapper; it
is never inferred from the work app. Privileged actions use fixed launchctl targets,
not shell commands assembled from user input. No password is stored. No Accessibility
permission is required by the app. If login/device approval is needed, the menu
reports it; perform login through the CLI rather than exposing account controls.

The menu app and daemon have separate lifecycles. Logging out of macOS stops the
user UI but leaves the system service running. Shutting down from the menu unloads
the service until started again or rebooted; it does not disable its boot policy.
To stop just the menu app, leaving networking alone:

```sh
launchctl bootout "gui/$(id -u)/io.github.zlobober.tailscale-companion.menubar"
```

Its LaunchAgent is
`~/Library/LaunchAgents/io.github.zlobober.tailscale-companion.menubar.plist`;
UI logs are under `~/Library/Logs/TailscaleCompanion/`. Running the installer again
rebuilds and replaces the UI. To disable its future login startup as well, use
`launchctl disable "gui/$(id -u)/io.github.zlobober.tailscale-companion.menubar"`
before unloading it. The installer re-enables it.

Build without installing: `go run ./cmd/menubar` produces
`dist/Tailscale Companion.app`. App bundles and Swift build caches are Git-ignored.
This is a locally signed build, not a notarized distribution. The menu uses an
original SF Symbols-based composition; no official Tailscale app artwork is copied.

Read-only diagnostics:

```sh
./tailscale-companion ui-status
"$HOME/Applications/Tailscale Companion.app/Contents/MacOS/TailscaleCompanionMenu" --status-json
```

## Routing behavior and limitations

Every three seconds, the monitor verifies the tailnet, MagicDNS suffix, and node IP,
discovers the unique active tunnel that owns that IP, and inspects the IPv4 routing
table.

- It adds the configured IPv4 prefix and `100.100.100.101/32`. A broader primary
  `/10` is left alone.
- Equal/more-specific routes within the prefix pointing elsewhere are conflicts.
- Existing unmanaged routes are not adopted, even on the same interface.
- Reconnects/interface changes remove the owned old route before adding the new one.
- Logout, wrong-tailnet selection, or orderly shutdown removes both owned routes if
  they still point to the recorded interface. Foreign replacement routes are preserved.
- Once the expected tailnet is verified, it writes `/etc/resolver/<MagicDNS-suffix>`
  pointing only that suffix at `100.100.100.101`; shutdown removes only an unchanged
  file bearing the companion marker. Existing foreign resolver files are never replaced.
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
- Secondary IPv6 and LAN subnet routing are deliberately absent. MagicDNS is
  available only for the companion tailnet's fully qualified `*.ts.net` suffix;
  short-name search and personal-tailnet DNS policy beyond MagicDNS are not installed.
  Do not add the shared Tailscale IPv6 `/48` to the secondary tunnel.
- The daemon config uses upstream's experimental `alpha0` format and is unlocked
  for interactive login. Do not override safety preferences through later CLI calls.
- Logs are plain files; rotation is not configured yet.

## Source map and tests

- `main.go`: CLI commands, configuration validation, supervision.
- `routes.go`: identity checks, tunnel discovery, route and split-DNS lifecycle.
- `service.go`: root-owned deployment and LaunchDaemon definition.
- `settings.go`: local identity/prefix settings and validation.
- `ui_status.go`: read-only connection/routing status for the unprivileged UI.
- `macos/`: native AppKit menu app, service client, and Swift tests.
- `cmd/menubar/`: Go app-bundle packager and per-user login-agent installer.
- `*.example.json`: publishable templates, not active configuration.
- `patches/`: pinned upstream daemon patch and license.
- `*_test.go`: fake route/CLI operations, lifecycle and conflict tests, configuration,
  deployment paths, and process selection. No live route changes are made by tests.

Run `go test -race ./...` and `go vet ./...` before deploying. On upstream
upgrades, review the macOS router, DNS service-IP, and netstack implementations
again: the private patch assumes explicit route operations are driven
by the filtered configuration and that all service-IP consumers use `tsaddr`'s central
value. Do not blindly replace the daemon with a stock binary.
