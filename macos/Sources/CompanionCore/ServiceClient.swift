import Foundation
import Darwin

public struct RoutedService: Decodable, Equatable, Sendable {
    public let name: String
    public let ipv4: String

    public init(name: String, ipv4: String) {
        self.name = name
        self.ipv4 = ipv4
    }
}

public struct Snapshot: Decodable, Equatable, Sendable {
    public let serviceInstalled: Bool
    public let serviceLoaded: Bool
    public let state: String
    public let detail: String
    public let tailnet: String?
    public let ipv4: String?
    public let interface: String?
    public let routeReady: Bool
    public let routedServices: [RoutedService]

    public init(serviceInstalled: Bool = false, serviceLoaded: Bool = false,
                state: String = "unavailable", detail: String,
                tailnet: String? = nil, ipv4: String? = nil,
                interface: String? = nil, routeReady: Bool = false,
                routedServices: [RoutedService] = []) {
        self.serviceInstalled = serviceInstalled
        self.serviceLoaded = serviceLoaded
        self.state = state
        self.detail = detail
        self.tailnet = tailnet
        self.ipv4 = ipv4
        self.interface = interface
        self.routeReady = routeReady
        self.routedServices = routedServices
    }

    private enum CodingKeys: String, CodingKey {
        case serviceInstalled, serviceLoaded, state, detail, tailnet, ipv4, interface, routeReady, routedServices
    }

    public init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        serviceInstalled = try values.decode(Bool.self, forKey: .serviceInstalled)
        serviceLoaded = try values.decode(Bool.self, forKey: .serviceLoaded)
        state = try values.decode(String.self, forKey: .state)
        detail = try values.decode(String.self, forKey: .detail)
        tailnet = try values.decodeIfPresent(String.self, forKey: .tailnet)
        ipv4 = try values.decodeIfPresent(String.self, forKey: .ipv4)
        interface = try values.decodeIfPresent(String.self, forKey: .interface)
        routeReady = try values.decode(Bool.self, forKey: .routeReady)
        routedServices = try values.decodeIfPresent([RoutedService].self, forKey: .routedServices) ?? []
    }

    public var isConnected: Bool { state == "connected" && routeReady }
    public var canStart: Bool { serviceInstalled && !serviceLoaded && state == "stopped" }
    public var canStop: Bool { serviceInstalled && serviceLoaded }
    public var title: String {
        switch state {
        case "connected": return routeReady ? "Connected" : "Routing not ready"
        case "stopped": return "Shut down"
        case "starting": return "Starting…"
        case "needs-login": return "Login required"
        case "degraded": return "Needs attention"
        case "not-installed": return "Service update required"
        default: return "Status unavailable"
        }
    }
}

public enum ServiceAction: Equatable, Sendable {
    case start, stop

    // Fixed targets only. No output/config/user input is interpolated into a root command.
    public var shellCommand: String {
        switch self {
        case .start:
            return "/bin/launchctl enable system/me.zlobober.tailscale-personal && /bin/launchctl bootstrap system /Library/LaunchDaemons/me.zlobober.tailscale-personal.plist"
        case .stop:
            return "/bin/launchctl bootout system/me.zlobober.tailscale-personal"
        }
    }
    public var appleScript: String {
        "do shell script \"\(shellCommand)\" with administrator privileges"
    }
}

public struct CommandResult: Sendable {
    public let code: Int32
    public let stdout: Data
    public let stderr: Data
    public let timedOut: Bool
    public init(code: Int32, stdout: Data = Data(), stderr: Data = Data(), timedOut: Bool = false) {
        self.code = code; self.stdout = stdout; self.stderr = stderr; self.timedOut = timedOut
    }
}

public protocol CommandRunning: Sendable {
    func run(_ executable: String, _ arguments: [String], timeout: TimeInterval) throws -> CommandResult
}

private final class DataBox: @unchecked Sendable {
    private let lock = NSLock()
    private var data = Data()
    func set(_ value: Data) { lock.lock(); defer { lock.unlock() }; data = value }
    func get() -> Data { lock.lock(); defer { lock.unlock() }; return data }
}

public final class ProcessRunner: CommandRunning {
    public init() {}
    public func run(_ executable: String, _ arguments: [String], timeout: TimeInterval) throws -> CommandResult {
        let process = Process()
        let output = Pipe(), errors = Pipe()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = output
        process.standardError = errors
        let exited = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in exited.signal() }
        try process.run()
        // Drain both pipes while the child runs; large status/error output must not deadlock it.
        let group = DispatchGroup()
        let stdout = DataBox(), stderr = DataBox()
        for (handle, box) in [(output.fileHandleForReading, stdout), (errors.fileHandleForReading, stderr)] {
            group.enter()
            DispatchQueue.global(qos: .utility).async {
                box.set(handle.readDataToEndOfFile())
                group.leave()
            }
        }
        let timedOut = exited.wait(timeout: .now() + timeout) == .timedOut
        if timedOut {
            process.terminate()
            if exited.wait(timeout: .now() + 2) == .timedOut {
                kill(process.processIdentifier, SIGKILL)
                _ = exited.wait(timeout: .now() + 2)
            }
        }
        // A detached privileged child may still hold pipes after osascript exits.
        // Bound this wait; the UI will refresh status rather than assume the action failed.
        _ = group.wait(timeout: .now() + 2)
        return CommandResult(code: process.isRunning ? -1 : process.terminationStatus,
                             stdout: stdout.get(), stderr: stderr.get(), timedOut: timedOut)
    }
}

public enum ActionResult: Equatable, Sendable {
    case success, cancelled, failure(String)
}

public final class ServiceClient: Sendable {
    public static let wrapper = "/usr/local/libexec/tailscale-personal/tailscale-companion"
    private let runner: CommandRunning
    public init(runner: CommandRunning = ProcessRunner()) { self.runner = runner }

    public func status() -> Snapshot {
        do {
            let result = try runner.run(Self.wrapper, ["ui-status"], timeout: 20)
            guard !result.timedOut, result.code == 0 else {
                return Snapshot(detail: "Cannot read the personal service status")
            }
            do {
                return try JSONDecoder().decode(Snapshot.self, from: result.stdout)
            } catch {
                return Snapshot(detail: "The service returned an unrecognized status format")
            }
        } catch {
            return Snapshot(state: "not-installed", detail: "Install or update the companion service first")
        }
    }

    public func perform(_ action: ServiceAction) -> ActionResult {
        // Recheck current state immediately before showing the system authorization prompt.
        let current = status()
        switch action {
        case .start:
            if current.serviceLoaded { return .success }
            guard current.canStart else { return .failure(current.detail) }
        case .stop:
            if current.serviceInstalled && !current.serviceLoaded && current.state == "stopped" { return .success }
            guard current.canStop else { return .failure(current.detail) }
        }
        do {
            let result = try runner.run("/usr/bin/osascript", ["-e", action.appleScript], timeout: 120)
            if result.timedOut { return .failure("Authorization or service control timed out. Check the current status before retrying.") }
            if result.code == 0 { return .success }
            let message = String(decoding: result.stderr, as: UTF8.self)
            if message.contains("(-128)") || message.contains("User canceled") { return .cancelled }
            // Another actor may have completed the requested operation during authorization.
            let updated = status()
            if action == .start && updated.serviceLoaded { return .success }
            if action == .stop && updated.state == "stopped" { return .success }
            return .failure(message.isEmpty ? "macOS could not change the personal service state." : String(message.prefix(1000)))
        } catch {
            return .failure(error.localizedDescription)
        }
    }
}
