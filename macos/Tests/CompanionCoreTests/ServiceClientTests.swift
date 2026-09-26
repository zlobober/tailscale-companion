import Foundation
import Testing
@testable import CompanionCore

// Each test owns its runner and uses it synchronously; production uses ProcessRunner.
private final class FakeRunner: CommandRunning, @unchecked Sendable {
    var calls: [(String, [String])] = []
    var results: [CommandResult]
    init(_ results: [CommandResult]) { self.results = results }
    func run(_ executable: String, _ arguments: [String], timeout: TimeInterval) throws -> CommandResult {
        calls.append((executable, arguments))
        guard !results.isEmpty else { throw NSError(domain: "unexpected-command", code: 1) }
        return results.removeFirst()
    }
}

struct ServiceClientTests {
    private func status(_ state: String, loaded: Bool, ready: Bool = false) -> CommandResult {
        let data = try! JSONSerialization.data(withJSONObject: [
            "serviceInstalled": true, "serviceLoaded": loaded, "state": state,
            "detail": "test status", "routeReady": ready
        ])
        return CommandResult(code: 0, stdout: data)
    }

    @Test func connectionRequiresRouting() {
        #expect(!Snapshot(state: "connected", detail: "", routeReady: false).isConnected)
        #expect(Snapshot(state: "connected", detail: "", routeReady: true).isConnected)
        #expect(Snapshot(serviceInstalled: true, state: "stopped", detail: "").canStart)
        #expect(!Snapshot(serviceInstalled: true, state: "unavailable", detail: "").canStart)
        #expect(Snapshot(serviceInstalled: true, serviceLoaded: true, state: "starting", detail: "").canStop)
        #expect(!Snapshot(state: "not-installed", detail: "").canStart)
    }

    @Test func decodesRoutedServices() {
        let data = try! JSONSerialization.data(withJSONObject: [
            "serviceInstalled": true, "serviceLoaded": true, "state": "connected",
            "detail": "ready", "routeReady": true,
            "routedServices": [["name": "svc:grafana", "ipv4": "100.99.151.209"]]
        ])
        let snapshot = try! JSONDecoder().decode(Snapshot.self, from: data)
        #expect(snapshot.routedServices == [RoutedService(name: "svc:grafana", ipv4: "100.99.151.209")])
    }

    @Test func readOnlyStatusUsesOnlyInstalledWrapper() {
        let runner = FakeRunner([status("connected", loaded: true, ready: true)])
        let result = ServiceClient(runner: runner).status()
        #expect(result.isConnected)
        #expect(runner.calls.count == 1)
        #expect(runner.calls[0].0 == ServiceClient.wrapper)
        #expect(runner.calls[0].1 == ["ui-status"])
    }

    @Test func startRequestsAuthorizationForFixedPersonalService() {
        let runner = FakeRunner([status("stopped", loaded: false), CommandResult(code: 0)])
        #expect(ServiceClient(runner: runner).perform(.start) == .success)
        #expect(runner.calls[1].0 == "/usr/bin/osascript")
        #expect(runner.calls[1].1 == ["-e", ServiceAction.start.appleScript])
        #expect(ServiceAction.start.appleScript.contains("with administrator privileges"))
        #expect(ServiceAction.start.shellCommand.contains("bootstrap system /Library/LaunchDaemons/me.zlobober.tailscale-personal.plist"))
        #expect(!ServiceAction.start.shellCommand.contains("/Users/"))
    }

    @Test func shutdownUsesBootoutNotProcessKill() {
        let runner = FakeRunner([status("connected", loaded: true, ready: true), CommandResult(code: 0)])
        #expect(ServiceClient(runner: runner).perform(.stop) == .success)
        #expect(runner.calls[1].1 == ["-e", ServiceAction.stop.appleScript])
        #expect(ServiceAction.stop.shellCommand == "/bin/launchctl bootout system/me.zlobober.tailscale-personal")
    }

    @Test func alreadyStartedIsIdempotent() {
        let runner = FakeRunner([status("starting", loaded: true)])
        #expect(ServiceClient(runner: runner).perform(.start) == .success)
        #expect(runner.calls.count == 1)
    }

    @Test func unknownStatusDoesNotAuthorizeMutation() {
        let runner = FakeRunner([status("unavailable", loaded: false)])
        if case .failure = ServiceClient(runner: runner).perform(.start) {} else { Issue.record("unsafe start") }
        #expect(runner.calls.count == 1)
    }

    @Test func cancelledAuthorizationIsNotAnError() {
        let runner = FakeRunner([status("stopped", loaded: false),
            CommandResult(code: 1, stderr: Data("User canceled. (-128)".utf8))])
        #expect(ServiceClient(runner: runner).perform(.start) == .cancelled)
    }

    @Test func timeoutDoesNotClaimSuccess() {
        let runner = FakeRunner([status("stopped", loaded: false), CommandResult(code: -1, timedOut: true)])
        if case .failure = ServiceClient(runner: runner).perform(.start) {} else { Issue.record("timeout claimed success") }
    }

    @Test func malformedStatusDisablesControls() {
        let runner = FakeRunner([CommandResult(code: 0, stdout: Data("not json".utf8))])
        let result = ServiceClient(runner: runner).status()
        #expect(result.state == "unavailable")
        #expect(!result.canStart)
        #expect(!result.canStop)
    }

    @Test func processRunnerDrainsLargeOutput() throws {
        let text = String(repeating: "x", count: 100_000)
        let result = try ProcessRunner().run("/usr/bin/printf", ["%s", text], timeout: 5)
        #expect(result.code == 0)
        #expect(result.stdout.count == text.count)
        #expect(!result.timedOut)
    }

    @Test func processRunnerTimeout() throws {
        let result = try ProcessRunner().run("/bin/sleep", ["5"], timeout: 0.05)
        #expect(result.timedOut)
    }
}
