import Foundation
import Combine

class ConfigStore: ObservableObject {
    static let shared = ConfigStore()

    private let configKey = "yellowsocks_yaml_config"
    @Published var configYAML: String = ""

    static let defaultConfig: String = """
# YellowSocks Configuration for iOS & iPadOS
log_level: "info"
web_port: 9090
tun_name: "tun-mobile"
enable_fake_ip: true
direct_dns: "1.1.1.1:53"
remote_doh: "https://1.1.1.1/dns-query"

nodes:
  - name: "Primary-Node"
    server: "1.2.3.4:8888"
    proto: "tcp"
    key: "123456"
    encrypt: "default"
    compress: 128

  - name: "Backup-Node"
    server: "5.6.7.8:8888"
    proto: "kcp"
    key: "123456"
    encrypt: "default"
    compress: 128

bypass_cidrs:
  - "10.0.0.0/8"
  - "172.16.0.0/12"
  - "192.168.0.0/16"
  - "127.0.0.0/8"
"""

    init() {
        loadConfig()
    }

    func loadConfig() {
        if let saved = UserDefaults.standard.string(forKey: configKey), !saved.isEmpty {
            self.configYAML = saved
        } else {
            self.configYAML = ConfigStore.defaultConfig
            saveConfig(ConfigStore.defaultConfig)
        }
    }

    func saveConfig(_ newConfig: String) {
        self.configYAML = newConfig
        UserDefaults.standard.set(newConfig, forKey: configKey)
    }

    func resetToDefault() {
        saveConfig(ConfigStore.defaultConfig)
    }
}
