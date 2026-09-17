import SwiftUI

@main
struct YellowSocksApp: App {
    @StateObject private var vpnManager = VPNManager.shared
    @StateObject private var configStore = ConfigStore.shared

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(vpnManager)
                .environmentObject(configStore)
                .onAppear {
                    vpnManager.loadManager()
                }
        }
    }
}
