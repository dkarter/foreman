import AppKit
import Darwin
import Foundation

private let serviceLabel = "dev.herdr.foreman"
private let dashboardURL = URL(string: "http://127.0.0.1:4040/")!
private let statusURL = URL(string: "http://127.0.0.1:4040/api/status")!
private let settingsURL = URL(string: "http://127.0.0.1:4040/api/settings")!
private let pairingURL = URL(string: "http://127.0.0.1:4040/api/pairing/control")!
private let pollIntervals = [5, 10, 30, 60]

private enum AgentSummary: String {
    case idle
    case done
    case working
    case blocked

    var color: NSColor {
        switch self {
        case .idle: return NSColor(red: 72 / 255, green: 213 / 255, blue: 151 / 255, alpha: 1)
        case .done: return NSColor(red: 89 / 255, green: 185 / 255, blue: 243 / 255, alpha: 1)
        case .working: return NSColor(red: 1, green: 188 / 255, blue: 66 / 255, alpha: 1)
        case .blocked: return NSColor(red: 1, green: 90 / 255, blue: 95 / 255, alpha: 1)
        }
    }
}

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
    private var allowPairingItem: NSMenuItem!
    private var pairingItem: NSMenuItem!
    private var pairedKiosksItem: NSMenuItem!
    private var unpairItem: NSMenuItem!
    private var healthTimer: Timer?
    private var blinkTimer: Timer?
    private var appearanceObservation: NSKeyValueObservation?
    private var lightStatusImage: NSImage?
    private var darkStatusImage: NSImage?
    private var interval = 5
    private var agentSummary: AgentSummary?
    private var indicatorVisible = true
    private var pendingPairingID: String?
    private var pendingPairingCode: String?
    private var pendingPairingName: String?

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        lightStatusImage = loadStatusImage(named: "foreman-menubar-for-light")
        darkStatusImage = loadStatusImage(named: "foreman-menubar-for-dark")
        appearanceObservation = statusItem.button?.observe(\.effectiveAppearance, options: [.initial, .new]) { [weak self] _, _ in
            DispatchQueue.main.async { self?.updateStatusIcon() }
        }
        statusItem.button?.toolTip = "Foreman"
        statusItem.menu = buildMenu()
        scheduleHealthPolling()
    }

    private func loadStatusImage(named resource: String) -> NSImage? {
        guard let imageURL = Bundle.main.url(forResource: resource, withExtension: "png"),
              let image = NSImage(contentsOf: imageURL) else { return nil }
        image.size = NSSize(width: 18, height: 18)
        image.accessibilityDescription = "Foreman"
        return image
    }

    private func updateStatusIcon() {
        guard let button = statusItem.button else { return }
        let darkMode = button.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        let baseImage = (darkMode ? darkStatusImage : lightStatusImage)
            ?? NSImage(systemSymbolName: "rectangle.on.rectangle", accessibilityDescription: "Foreman")
        guard let baseImage, let agentSummary, indicatorVisible else {
            button.image = baseImage
            return
        }

        let image = NSImage(size: NSSize(width: 18, height: 18))
        image.lockFocus()
        baseImage.draw(in: NSRect(x: 0, y: 0, width: 18, height: 18))
        let badge = NSBezierPath(ovalIn: NSRect(x: 0.5, y: 0.5, width: 5, height: 5))
        (darkMode ? NSColor.black : NSColor.white).setStroke()
        badge.lineWidth = 1
        agentSummary.color.setFill()
        badge.fill()
        badge.stroke()
        image.unlockFocus()
        image.accessibilityDescription = "Foreman, agents \(agentSummary.rawValue)"
        button.image = image
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

        allowPairingItem = item("Allow New Kiosk...", #selector(enablePairing))
        pairingItem = item("Pairing: Checking...", #selector(reviewPairing))
        pairingItem.isEnabled = false
        unpairItem = item("Forget Paired Kiosks...", #selector(unpairAllKiosks))
        unpairItem.isEnabled = false
        pairedKiosksItem = NSMenuItem(title: "Paired Kiosks", action: nil, keyEquivalent: "")
        pairedKiosksItem.submenu = NSMenu()
        menu.addItem(allowPairingItem)
        menu.addItem(pairingItem)
        menu.addItem(pairedKiosksItem)
        menu.addItem(unpairItem)
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
        healthTimer?.invalidate()
        healthTimer = Timer.scheduledTimer(timeInterval: 5, target: self, selector: #selector(checkHealth), userInfo: nil, repeats: true)
        RunLoop.main.add(healthTimer!, forMode: .common)
        checkHealth()
    }

    @objc private func checkHealth() {
        var request = URLRequest(url: statusURL)
        request.timeoutInterval = 2
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            let healthy = (response as? HTTPURLResponse)?.statusCode == 200
            let state: [String: Any]? = data.flatMap {
                try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
            }
            let seconds = state?["pollIntervalSeconds"] as? Int
            let summary = (state?["agentStatus"] as? String).flatMap(AgentSummary.init(rawValue:))
            DispatchQueue.main.async {
                self?.setRunning(healthy)
                self?.setAgentSummary(healthy ? summary : nil)
                if let seconds {
                    self?.interval = seconds
                    self?.updatePollingChecks()
                }
            }
        }.resume()
        checkPairing()
    }

    private func setAgentSummary(_ summary: AgentSummary?) {
        guard agentSummary != summary else { return }
        agentSummary = summary
        indicatorVisible = true
        blinkTimer?.invalidate()
        blinkTimer = nil
        if summary == .blocked {
            blinkTimer = Timer.scheduledTimer(timeInterval: 0.55, target: self, selector: #selector(toggleStatusIndicator), userInfo: nil, repeats: true)
            RunLoop.main.add(blinkTimer!, forMode: .common)
        }
        updateStatusIcon()
    }

    @objc private func toggleStatusIndicator() {
        indicatorVisible.toggle()
        updateStatusIcon()
    }

    private func checkPairing() {
        var request = URLRequest(url: pairingURL)
        request.timeoutInterval = 2
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            guard (response as? HTTPURLResponse)?.statusCode == 200,
                  let data,
                  let state = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else { return }
            let pending = state["pending"] as? [String: Any]
            let devices = state["devices"] as? [[String: Any]] ?? []
            let pairingEnabled = state["pairingEnabled"] as? Bool == true
            DispatchQueue.main.async {
                self?.pendingPairingID = pending?["id"] as? String
                self?.pendingPairingCode = pending?["code"] as? String
                self?.pendingPairingName = pending?["name"] as? String
                if let code = self?.pendingPairingCode {
                    self?.pairingItem.title = "Approve Pairing · \(Self.formatCode(code))"
                    self?.pairingItem.isEnabled = true
                } else {
                    self?.pairingItem.title = "No Pending Pairing Request"
                    self?.pairingItem.isEnabled = false
                }
                self?.allowPairingItem.title = pairingEnabled ? "Pairing Enabled for 3 Minutes" : "Allow New Kiosk..."
                self?.unpairItem.title = "Forget Paired Kiosks... (\(devices.count))"
                self?.unpairItem.isEnabled = !devices.isEmpty
                let kioskMenu = NSMenu()
                for device in devices {
                    guard let id = device["id"] as? String else { continue }
                    let name = device["name"] as? String ?? "Foreman kiosk"
                    let kioskItem = self?.item("Forget \(name)...", #selector(AppDelegate.unpairKiosk(_:)))
                    kioskItem?.representedObject = id
                    if let kioskItem { kioskMenu.addItem(kioskItem) }
                }
                self?.pairedKiosksItem.submenu = kioskMenu
                self?.pairedKiosksItem.isEnabled = !devices.isEmpty
            }
        }.resume()
    }

    @objc private func enablePairing() {
        var request = URLRequest(url: pairingURL)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try? JSONSerialization.data(withJSONObject: ["action": "enable"])
        URLSession.shared.dataTask(with: request) { [weak self] _, _, _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { self?.checkPairing() }
        }.resume()
    }

    private static func formatCode(_ code: String) -> String {
        guard code.count == 6 else { return code }
        let midpoint = code.index(code.startIndex, offsetBy: 3)
        return "\(code[..<midpoint]) \(code[midpoint...])"
    }

    @objc private func reviewPairing() {
        guard let id = pendingPairingID, let code = pendingPairingCode else { return }
        let alert = NSAlert()
        alert.messageText = "Pair with \(pendingPairingName ?? "Foreman kiosk")?"
        alert.informativeText = "Confirm this code is also displayed on the kiosk:\n\n\(Self.formatCode(code))"
        alert.alertStyle = .informational
        alert.addButton(withTitle: "Approve")
        alert.addButton(withTitle: "Reject")
        alert.addButton(withTitle: "Cancel")
        let response = alert.runModal()
        if response == .alertFirstButtonReturn {
            sendPairingDecision(id: id, approve: true)
        } else if response == .alertSecondButtonReturn {
            sendPairingDecision(id: id, approve: false)
        }
    }

    private func sendPairingDecision(id: String, approve: Bool) {
        var request = URLRequest(url: pairingURL)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try? JSONSerialization.data(withJSONObject: ["id": id, "approve": approve])
        URLSession.shared.dataTask(with: request) { [weak self] _, _, _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { self?.checkPairing() }
        }.resume()
    }

    @objc private func unpairAllKiosks() {
        confirmUnpair(deviceID: nil, name: "all paired kiosks")
    }

    @objc private func unpairKiosk(_ sender: NSMenuItem) {
        guard let deviceID = sender.representedObject as? String else { return }
        confirmUnpair(deviceID: deviceID, name: sender.title.replacingOccurrences(of: "Forget ", with: "").replacingOccurrences(of: "...", with: ""))
    }

    private func confirmUnpair(deviceID: String?, name: String) {
        let alert = NSAlert()
        alert.messageText = "Forget \(name)?"
        alert.informativeText = "Access will be revoked immediately. Pairing again will require a new code and approval on both devices."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Unpair")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        var components = URLComponents(url: pairingURL, resolvingAgainstBaseURL: false)!
        if let deviceID {
            components.queryItems = [URLQueryItem(name: "device", value: deviceID)]
        }
        var request = URLRequest(url: components.url!)
        request.httpMethod = "DELETE"
        URLSession.shared.dataTask(with: request) { [weak self] _, _, _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { self?.checkPairing() }
        }.resume()
    }

    private func setRunning(_ value: Bool) {
        statusLine.title = value ? "Foreman: Running" : "Foreman: Stopped"
        startItem.isEnabled = !value
        stopItem.isEnabled = value
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
