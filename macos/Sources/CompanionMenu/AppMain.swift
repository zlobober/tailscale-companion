import AppKit
import CompanionCore

@MainActor
private enum MenuIcon {
    static func image(connected: Bool, attention: Bool, busy: Bool) -> NSImage {
        // Native house silhouette + small state badge: distinct from Tailscale's dots.
        // Template rendering automatically follows light/dark/high-contrast appearance.
        let house = NSImage(systemSymbolName: connected ? "house.fill" : "house",
                            accessibilityDescription: "Personal Tailscale")!
        let image = NSImage(size: NSSize(width: 25, height: 18), flipped: false) { _ in
            house.draw(in: NSRect(x: 0, y: 1, width: 17, height: 16))
            NSColor.black.set()
            let badge = NSBezierPath(ovalIn: NSRect(x: 19, y: 1, width: 5, height: 5))
            if connected {
                badge.fill()
            } else {
                badge.lineWidth = 1
                badge.stroke()
                if attention || busy {
                    let mark = NSBezierPath()
                    mark.move(to: NSPoint(x: 21.5, y: 7.5))
                    mark.line(to: NSPoint(x: 21.5, y: 12))
                    mark.lineWidth = 1.5
                    mark.stroke()
                }
            }
            return true
        }
        image.isTemplate = true
        return image
    }
}

@MainActor
private final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private let client = ServiceClient()
    private var statusItem: NSStatusItem!
    private let menu = NSMenu()
    private let connectionItem = NSMenuItem(title: "Status: Checking…", action: nil, keyEquivalent: "")
    private let addressItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private let routingItem = NSMenuItem(title: "", action: nil, keyEquivalent: "")
    private let startItem = NSMenuItem(title: "Start Personal Tailscale", action: #selector(startService), keyEquivalent: "")
    private let stopItem = NSMenuItem(title: "Shut Down Personal Tailscale", action: #selector(stopService), keyEquivalent: "")
    private var snapshot = Snapshot(detail: "Checking the personal service")
    private var timer: Timer?
    private var refreshing = false
    private var statusGeneration = 0
    private var pendingAction: ServiceAction?

    func applicationDidFinishLaunching(_ notification: Notification) {
        if let id = Bundle.main.bundleIdentifier,
           NSRunningApplication.runningApplications(withBundleIdentifier: id).contains(where: {
               $0.processIdentifier != ProcessInfo.processInfo.processIdentifier
           }) {
            NSApp.terminate(nil)
            return
        }
        NSApp.setActivationPolicy(.accessory)
        menu.autoenablesItems = false
        menu.delegate = self
        let heading = NSMenuItem(title: "Personal Tailscale", action: nil, keyEquivalent: "")
        heading.attributedTitle = NSAttributedString(string: heading.title,
            attributes: [.font: NSFont.boldSystemFont(ofSize: NSFont.systemFontSize)])
        heading.isEnabled = false
        menu.addItem(heading)
        for item in [connectionItem, addressItem, routingItem] {
            item.isEnabled = false
            menu.addItem(item)
        }
        menu.addItem(.separator())
        for item in [startItem, stopItem] {
            item.target = self
            menu.addItem(item)
        }
        startItem.image = NSImage(systemSymbolName: "play.fill", accessibilityDescription: nil)
        stopItem.image = NSImage(systemSymbolName: "stop.fill", accessibilityDescription: nil)
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.menu = menu
        statusItem.button?.imagePosition = .imageOnly
        render()
        refresh()
        let timer = Timer(timeInterval: 5, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in self?.refresh() }
        }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    func applicationWillTerminate(_ notification: Notification) {
        timer?.invalidate()
        // Quitting/logging out of the UI never implicitly stops the system daemon.
    }

    func menuWillOpen(_ menu: NSMenu) { refresh() }

    private func refresh() {
        guard !refreshing, pendingAction == nil else { return }
        refreshing = true
        let generation = statusGeneration
        let client = self.client
        DispatchQueue.global(qos: .utility).async { [weak self] in
            let result = client.status()
            DispatchQueue.main.async { [weak self] in
                guard let self else { return }
                // Ignore a poll begun before a Start/Shutdown action.
                if self.statusGeneration == generation { self.snapshot = result }
                self.refreshing = false
                self.render()
            }
        }
    }

    private func render() {
        let busy = pendingAction != nil
        let title: String
        if let pendingAction {
            title = pendingAction == .start ? "Starting…" : "Shutting down…"
        } else {
            title = snapshot.title
        }
        connectionItem.title = "Status: \(title)"
        connectionItem.toolTip = snapshot.detail
        addressItem.isHidden = snapshot.ipv4 == nil
        addressItem.title = snapshot.ipv4.map { "IP: \($0)" } ?? ""
        addressItem.toolTip = snapshot.tailnet
        routingItem.isHidden = !snapshot.serviceLoaded
        routingItem.title = snapshot.routeReady ? "Routing: Ready" : "Routing: Not ready"
        routingItem.toolTip = snapshot.detail
        startItem.isEnabled = !busy && snapshot.canStart
        stopItem.isEnabled = !busy && snapshot.canStop
        let attention = ["degraded", "needs-login", "unavailable", "not-installed"].contains(snapshot.state)
        statusItem.button?.image = MenuIcon.image(connected: snapshot.isConnected && !busy,
                                                  attention: attention, busy: busy)
        statusItem.button?.toolTip = "Tailscale Companion — \(title)\n\(snapshot.detail)"
        statusItem.button?.setAccessibilityLabel("Personal Tailscale: \(title)")
    }

    @objc private func startService() { perform(.start) }
    @objc private func stopService() { perform(.stop) }

    private func perform(_ action: ServiceAction) {
        guard pendingAction == nil else { return }
        pendingAction = action
        statusGeneration &+= 1
        render()
        let client = self.client
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            let result = client.perform(action)
            let updated = client.status()
            DispatchQueue.main.async { [weak self] in
                guard let self else { return }
                self.pendingAction = nil
                self.snapshot = updated
                self.render()
                if case .failure(let message) = result {
                    NSApp.activate(ignoringOtherApps: true)
                    let alert = NSAlert()
                    alert.messageText = "Could not change Personal Tailscale"
                    alert.informativeText = message
                    alert.alertStyle = .warning
                    alert.addButton(withTitle: "OK")
                    alert.runModal()
                }
                self.refresh()
            }
        }
    }
}

@main
private enum AppMain {
    @MainActor static func main() {
        // Read-only diagnostics for packaging/smoke tests; no extra menu features.
        if CommandLine.arguments.contains("--status-json") {
            let result = ServiceClient().status()
            let fields: [String: Any] = ["state": result.state, "detail": result.detail,
                "serviceLoaded": result.serviceLoaded, "routeReady": result.routeReady]
            if let data = try? JSONSerialization.data(withJSONObject: fields, options: [.sortedKeys]),
               let text = String(data: data, encoding: .utf8) { print(text) }
            return
        }
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        withExtendedLifetime(delegate) { app.run() }
    }
}
