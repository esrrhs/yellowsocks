import Foundation
import NetworkExtension
import Combine

class VPNManager: ObservableObject {
    static let shared = VPNManager()

    @Published var isConnected: Bool = false
    @Published var statusText: String = "Disconnected"
    @Published var uploadSpeedText: String = "0 B/s"
    @Published var downloadSpeedText: String = "0 B/s"
    @Published var totalUploadText: String = "0 MB"
    @Published var totalDownloadText: String = "0 MB"
    @Published var activeNodeText: String = "No Active Node"
    @Published var latencyText: String = "-- ms"

    private var providerManager: NETunnelProviderManager?
    private var statusObserver: Any?
    private var timer: Timer?

    private let appGroupID = "group.com.esrrhs.yellowsocks"

    private init() {
        setupObserver()
        startStatsPoll()
    }

    deinit {
        if let observer = statusObserver {
            NotificationCenter.default.removeObserver(observer)
        }
        timer?.invalidate()
    }

    func loadManager(completion: (() -> Void)? = nil) {
        NETunnelProviderManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self = self else { return }
            if let mgr = managers?.first {
                self.providerManager = mgr
            } else {
                let newMgr = NETunnelProviderManager()
                let proto = NETunnelProviderProtocol()
                proto.providerBundleIdentifier = "com.esrrhs.yellowsocks.tunnel"
                proto.serverAddress = "127.0.0.1"
                newMgr.protocolConfiguration = proto
                newMgr.localizedDescription = "YellowSocks"
                newMgr.isEnabled = true
                self.providerManager = newMgr
            }
            self.updateStatus()
            completion?()
        }
    }

    private func setupObserver() {
        statusObserver = NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            self?.updateStatus()
        }
    }

    private func updateStatus() {
        guard let connection = providerManager?.connection else {
            self.isConnected = false
            self.statusText = "Disconnected"
            return
        }

        switch connection.status {
        case .connected:
            self.isConnected = true
            self.statusText = "Connected"
        case .connecting:
            self.isConnected = false
            self.statusText = "Connecting..."
        case .disconnecting:
            self.isConnected = false
            self.statusText = "Disconnecting..."
        case .disconnected:
            self.isConnected = false
            self.statusText = "Disconnected"
        case .invalid:
            self.isConnected = false
            self.statusText = "Invalid Config"
        case .reasserting:
            self.isConnected = true
            self.statusText = "Reconnecting..."
        @unknown default:
            self.isConnected = false
            self.statusText = "Unknown"
        }
    }

    func toggleVPN(configContent: String) {
        if isConnected {
            stopVPN()
        } else {
            startVPN(configContent: configContent)
        }
    }

    func startVPN(configContent: String) {
        loadManager { [weak self] in
            guard let self = self, let mgr = self.providerManager else { return }

            mgr.isEnabled = true
            let proto = (mgr.protocolConfiguration as? NETunnelProviderProtocol) ?? NETunnelProviderProtocol()
            proto.providerBundleIdentifier = "com.esrrhs.yellowsocks.tunnel"
            proto.serverAddress = "127.0.0.1"
            proto.providerConfiguration = [
                "configContent": configContent
            ]
            mgr.protocolConfiguration = proto

            mgr.saveToPreferences { error in
                if let error = error {
                    DispatchQueue.main.async {
                        self.statusText = "Save Error: \(error.localizedDescription)"
                    }
                    return
                }

                mgr.loadFromPreferences { error in
                    if let error = error {
                        DispatchQueue.main.async {
                            self.statusText = "Reload Error: \(error.localizedDescription)"
                        }
                        return
                    }

                    do {
                        try mgr.connection.startVPNTunnel(options: nil)
                    } catch {
                        DispatchQueue.main.async {
                            self.statusText = "Start Error: \(error.localizedDescription)"
                        }
                    }
                }
            }
        }
    }

    func stopVPN() {
        providerManager?.connection.stopVPNTunnel()
    }

    private func startStatsPoll() {
        timer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            self?.pollSharedStats()
        }
    }

    private func pollSharedStats() {
        guard let sharedDefaults = UserDefaults(suiteName: appGroupID), isConnected else {
            return
        }

        let upBps = sharedDefaults.integer(forKey: "upload_bps")
        let downBps = sharedDefaults.integer(forKey: "download_bps")
        let totalUp = sharedDefaults.integer(forKey: "total_upload")
        let totalDown = sharedDefaults.integer(forKey: "total_download")
        let nodeName = sharedDefaults.string(forKey: "active_node_name") ?? "Default"
        let latency = sharedDefaults.integer(forKey: "active_node_latency")

        DispatchQueue.main.async {
            self.uploadSpeedText = self.formatBytes(upBps) + "/s"
            self.downloadSpeedText = self.formatBytes(downBps) + "/s"
            self.totalUploadText = self.formatBytes(totalUp)
            self.totalDownloadText = self.formatBytes(totalDown)
            self.activeNodeText = nodeName
            self.latencyText = latency > 0 ? "\(latency) ms" : "-- ms"
        }
    }

    private func formatBytes(_ bytes: Int) -> String {
        let b = Double(bytes)
        if b < 1024 { return String(format: "%.0f B", b) }
        let kb = b / 1024
        if kb < 1024 { return String(format: "%.1f KB", kb) }
        let mb = kb / 1024
        if mb < 1024 { return String(format: "%.1f MB", mb) }
        let gb = mb / 1024
        return String(format: "%.2f GB", gb)
    }
}
