import NetworkExtension
import Foundation
#if canImport(YellowSocks)
import YellowSocks
#endif

class PacketTunnelProvider: NEPacketTunnelProvider {

    private let appGroupID = "group.com.esrrhs.yellowsocks"
    private var isEngineStarted = false

    override func startTunnel(options: [String : NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        NSLog("[YellowSocksTunnel] Starting Packet Tunnel Provider...")

        // 1. 从主 App 传入的 protocolConfiguration 读取配置
        let proto = self.protocolConfiguration as? NETunnelProviderProtocol
        let configContent = (proto?.providerConfiguration?["configContent"] as? String) ?? ""

        // 2. 配置系统的虚拟网卡网络参数
        let tunnelNetworkSettings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")

        // IPv4 路由：虚拟 IP 10.255.0.2/24，路由默认接管所有 IPv4
        let ipv4Settings = NEIPv4Settings(addresses: ["10.255.0.2"], subnetMasks: ["255.255.255.0"])
        ipv4Settings.includedRoutes = [NEIPv4Route.default()]
        tunnelNetworkSettings.ipv4Settings = ipv4Settings

        // DNS 设置：接管系统所有 DNS 请求（Fake-IP 智能拦截模式）
        let dnsSettings = NEDNSSettings(servers: ["198.18.0.1", "1.1.1.1"])
        dnsSettings.matchDomains = [""]
        tunnelNetworkSettings.dnsSettings = dnsSettings

        tunnelNetworkSettings.mtu = 1500

        // 3. 应用网络配置
        setTunnelNetworkSettings(tunnelNetworkSettings) { [weak self] error in
            guard let self = self else { return }

            if let error = error {
                NSLog("[YellowSocksTunnel] Failed to set network settings: \(error.localizedDescription)")
                completionHandler(error)
                return
            }

            NSLog("[YellowSocksTunnel] Network settings applied successfully. Launching Go Core...")

            // 4. 获取底层的 utun 文件描述符
            var tunFd: Int32 = -1
            if let fdNum = self.packetFlow.value(forKeyPath: "socket.fileDescriptor") as? NSNumber {
                tunFd = fdNum.int32Value
            }

            #if canImport(YellowSocks)
            let callbackHandler = MobileCallbackHandler(appGroupID: self.appGroupID)
            var startErr: NSError?
            MobileStartEngine(Int(tunFd), configContent, callbackHandler, &startErr)

            if let startErr = startErr {
                NSLog("[YellowSocksTunnel] Go core failed to start: \(startErr.localizedDescription)")
                completionHandler(startErr)
                return
            }
            #endif

            self.isEngineStarted = true
            NSLog("[YellowSocksTunnel] YellowSocks Go core successfully started with tunFd=\(tunFd)")
            completionHandler(nil)
        }
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        NSLog("[YellowSocksTunnel] Stopping Packet Tunnel Provider, reason: \(reason.rawValue)")

        #if canImport(YellowSocks)
        if isEngineStarted {
            var stopErr: NSError?
            MobileStopEngine(&stopErr)
            isEngineStarted = false
        }
        #endif

        // 清理共享状态
        if let sharedDefaults = UserDefaults(suiteName: appGroupID) {
            sharedDefaults.set(0, forKey: "upload_bps")
            sharedDefaults.set(0, forKey: "download_bps")
        }

        completionHandler()
    }

    override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
        // 处理主 App 发送的 IPC 消息（如请求状态、切换节点）
        completionHandler?(nil)
    }

    override func sleep(completionHandler: @escaping () -> Void) {
        completionHandler()
    }

    override func wake() {
    }
}

#if canImport(YellowSocks)
// 实现 Go mobile Callback 协议
class MobileCallbackHandler: NSObject, MobileCallbackProtocol {
    private let appGroupID: String

    init(appGroupID: String) {
        self.appGroupID = appGroupID
        super.init()
    }

    func onStatusChanged(_ running: Bool, message: String?) {
        NSLog("[YellowSocksTunnel] StatusChanged: running=\(running), msg=\(message ?? "")")
    }

    func onBandwidthUpdate(_ uploadBps: Int64, downloadBps: Int64, totalUpload: Int64, totalDownload: Int64) {
        guard let sharedDefaults = UserDefaults(suiteName: appGroupID) else { return }
        sharedDefaults.set(Int(uploadBps), forKey: "upload_bps")
        sharedDefaults.set(Int(downloadBps), forKey: "download_bps")
        sharedDefaults.set(Int(totalUpload), forKey: "total_upload")
        sharedDefaults.set(Int(totalDownload), forKey: "total_download")
    }

    func onActiveNodeChanged(_ name: String?, server: String?, proto: String?, latencyMs: Int64) {
        guard let sharedDefaults = UserDefaults(suiteName: appGroupID) else { return }
        sharedDefaults.set(name ?? "Unknown", forKey: "active_node_name")
        sharedDefaults.set(Int(latencyMs), forKey: "active_node_latency")
    }
}
#endif
