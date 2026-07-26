import AppKit
import Darwin
import Foundation

private let serviceLabel = "dev.herdr.foreman"
private let dashboardURL = URL(string: "http://127.0.0.1:4040/")!
private let settingsURL = URL(string: "http://127.0.0.1:4040/api/settings")!
private let pollIntervals = [5, 10, 30, 60]

private func runLaunchctl(_ arguments: [String]) -> (Bool, String) {
    let process = Process()
    let output = Pipe()
    process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
    process.arguments = arguments
    process.standardOutput = output
    process.standardError = output
    do {
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        return (process.terminationStatus == 0, String(decoding: data, as: UTF8.self))
    } catch {
        return (false, error.localizedDescription)
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem!
    private var statusLine: NSMenuItem!
    private var startItem: NSMenuItem!
    private var stopItem: NSMenuItem!
    private var pollingItems: [NSMenuItem] = []
    private var timer: Timer?
    private var interval = 5

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem.button?.image = NSImage(systemSymbolName: "rectangle.on.rectangle", accessibilityDescription: "Foreman")
        statusItem.button?.toolTip = "Foreman"
        statusItem.menu = buildMenu()
        scheduleHealthPolling()
    }

    private func buildMenu() -> NSMenu {
        let menu = NSMenu()
        statusLine = NSMenuItem(title: "Foreman: Checking...", action: nil, keyEquivalent: "")
        statusLine.isEnabled = false
        menu.addItem(statusLine)
        menu.addItem(.separator())

        menu.addItem(item("Open Dashboard", #selector(openDashboard)))
        startItem = item("Start Foreman", #selector(startForeman))
        stopItem = item("Stop Foreman", #selector(stopForeman))
        menu.addItem(startItem)
        menu.addItem(stopItem)

        let pollingMenu = NSMenu()
        pollingItems = pollIntervals.map { seconds in
            let pollingItem = item("\(seconds) seconds", #selector(setPollingInterval(_:)))
            pollingItem.tag = seconds
            pollingMenu.addItem(pollingItem)
            return pollingItem
        }
        let pollingRoot = NSMenuItem(title: "Resource Polling", action: nil, keyEquivalent: "")
        pollingRoot.submenu = pollingMenu
        menu.addItem(pollingRoot)
        menu.addItem(.separator())
        menu.addItem(item("Quit Menu Bar", #selector(quitMenuBar)))
        menu.addItem(item("Quit Foreman", #selector(quitForeman)))
        return menu
    }

    private func item(_ title: String, _ action: Selector) -> NSMenuItem {
        let menuItem = NSMenuItem(title: title, action: action, keyEquivalent: "")
        menuItem.target = self
        return menuItem
    }

    private func scheduleHealthPolling() {
        timer?.invalidate()
        timer = Timer.scheduledTimer(timeInterval: 5, target: self, selector: #selector(checkHealth), userInfo: nil, repeats: true)
        RunLoop.main.add(timer!, forMode: .common)
        checkHealth()
    }

    @objc private func checkHealth() {
        var request = URLRequest(url: settingsURL)
        request.timeoutInterval = 2
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            let healthy = (response as? HTTPURLResponse)?.statusCode == 200
            let seconds = data.flatMap { data in
                (try? JSONSerialization.jsonObject(with: data) as? [String: Any])?["pollIntervalSeconds"] as? Int
            }
            DispatchQueue.main.async {
                self?.setRunning(healthy)
                if let seconds {
                    self?.interval = seconds
                    self?.updatePollingChecks()
                }
            }
        }.resume()
    }

    private func setRunning(_ value: Bool) {
        statusLine.title = value ? "Foreman: Running" : "Foreman: Stopped"
        startItem.isEnabled = !value
        stopItem.isEnabled = value
        statusItem.button?.contentTintColor = value ? .systemGreen : .secondaryLabelColor
        statusItem.button?.toolTip = statusLine.title
    }

    private func updatePollingChecks() {
        pollingItems.forEach { $0.state = $0.tag == interval ? .on : .off }
    }

    @objc private func openDashboard() {
        NSWorkspace.shared.open(dashboardURL)
    }

    @objc private func startForeman() {
        let uid = String(getuid())
        let target = "gui/\(uid)/\(serviceLabel)"
        let plist = FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/LaunchAgents/\(serviceLabel).plist").path
        runControl {
            if runLaunchctl(["print", target]).0 {
                return runLaunchctl(["kickstart", "-k", target])
            }
            return runLaunchctl(["bootstrap", "gui/\(uid)", plist])
        }
    }

    @objc private func stopForeman() {
        let target = "gui/\(getuid())/\(serviceLabel)"
        runControl { runLaunchctl(["bootout", target]) }
    }

    @objc private func setPollingInterval(_ sender: NSMenuItem) {
        guard pollIntervals.contains(sender.tag) else { return }
        interval = sender.tag
        updatePollingChecks()
        var request = URLRequest(url: settingsURL)
        request.httpMethod = "PUT"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try? JSONSerialization.data(withJSONObject: ["pollIntervalSeconds": interval])
        URLSession.shared.dataTask(with: request).resume()
    }

    private func runControl(_ operation: @escaping @Sendable () -> (Bool, String)) {
        statusLine.title = "Foreman: Updating..."
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            let result = operation()
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.4) {
                if !result.0 {
                    let detail = result.1.trimmingCharacters(in: .whitespacesAndNewlines)
                    self?.statusLine.title = detail.isEmpty ? "Foreman: Operation failed" : "Foreman: \(detail)"
                }
                self?.checkHealth()
            }
        }
    }

    @objc private func quitMenuBar() {
        NSApp.terminate(nil)
    }

    @objc private func quitForeman() {
        let target = "gui/\(getuid())/\(serviceLabel)"
        DispatchQueue.global(qos: .userInitiated).async {
            _ = runLaunchctl(["bootout", target])
            DispatchQueue.main.async { NSApp.terminate(nil) }
        }
    }
}

@main
struct ForemanStatusApp {
    @MainActor
    static func main() {
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        app.run()
        _ = delegate
    }
}
