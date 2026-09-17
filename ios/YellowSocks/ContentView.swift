import SwiftUI

struct ContentView: View {
    @EnvironmentObject var vpnManager: VPNManager
    @EnvironmentObject var configStore: ConfigStore

    @State private var showingConfigEditor = false
    @State private var editingYAML = ""

    var body: some View {
        NavigationView {
            ZStack {
                Color(UIColor.systemGroupedBackground)
                    .ignoresSafeArea()

                ScrollView {
                    VStack(spacing: 20) {
                        // Connection Header Card
                        headerCard

                        // Bandwidth Monitor Cards
                        bandwidthGrid

                        // Active Node & Latency Card
                        nodeStatusCard

                        // Quick Settings & Features Card
                        featuresCard

                        // Actions
                        actionButtons
                    }
                    .padding()
                }
            }
            .navigationTitle("YellowSocks")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button(action: {
                        editingYAML = configStore.configYAML
                        showingConfigEditor = true
                    }) {
                        Image(systemName: "slider.horizontal.3")
                            .imageScale(.medium)
                    }
                }
            }
            .sheet(isPresented: $showingConfigEditor) {
                configEditorSheet
            }
        }
        .navigationViewStyle(StackNavigationViewStyle())
    }

    // MARK: - Header & Main Toggle
    private var headerCard: some View {
        VStack(spacing: 16) {
            ZStack {
                Circle()
                    .fill(vpnManager.isConnected ? Color.green.opacity(0.15) : Color.gray.opacity(0.15))
                    .frame(width: 140, height: 140)

                Button(action: {
                    let impact = UIImpactFeedbackGenerator(style: .medium)
                    impact.impactOccurred()
                    vpnManager.toggleVPN(configContent: configStore.configYAML)
                }) {
                    ZStack {
                        Circle()
                            .fill(
                                LinearGradient(
                                    colors: vpnManager.isConnected
                                        ? [Color.green, Color.teal]
                                        : [Color.blue, Color.indigo],
                                    startPoint: .topLeading,
                                    endPoint: .bottomTrailing
                                )
                            )
                            .frame(width: 110, height: 110)
                            .shadow(color: vpnManager.isConnected ? Color.green.opacity(0.4) : Color.blue.opacity(0.3), radius: 10, y: 5)

                        Image(systemName: "power")
                            .font(.system(size: 42, weight: .bold))
                            .foregroundColor(.white)
                    }
                }
            }
            .padding(.top, 8)

            VStack(spacing: 4) {
                Text(vpnManager.statusText)
                    .font(.title3)
                    .fontWeight(.bold)
                    .foregroundColor(vpnManager.isConnected ? .green : .primary)

                Text(vpnManager.isConnected ? "System-wide TUN Proxy Active" : "Tap power button to connect")
                    .font(.caption)
                    .foregroundColor(.secondary)
            }
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 20)
        .background(Color(UIColor.secondarySystemGroupedBackground))
        .cornerRadius(18)
        .shadow(color: Color.black.opacity(0.04), radius: 5, y: 2)
    }

    // MARK: - Bandwidth Monitor Grid
    private var bandwidthGrid: some View {
        LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 12) {
            speedCard(title: "Upload Speed", value: vpnManager.uploadSpeedText, icon: "arrow.up.circle.fill", color: .blue)
            speedCard(title: "Download Speed", value: vpnManager.downloadSpeedText, icon: "arrow.down.circle.fill", color: .green)
            speedCard(title: "Total Upload", value: vpnManager.totalUploadText, icon: "icloud.and.arrow.up.fill", color: .orange)
            speedCard(title: "Total Download", value: vpnManager.totalDownloadText, icon: "icloud.and.arrow.down.fill", color: .purple)
        }
    }

    private func speedCard(title: String, value: String, icon: String, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Image(systemName: icon)
                    .foregroundColor(color)
                    .font(.system(size: 14))
                Text(title)
                    .font(.caption)
                    .foregroundColor(.secondary)
            }

            Text(value)
                .font(.system(size: 18, weight: .bold, design: .monospaced))
                .foregroundColor(.primary)
                .minimumScaleFactor(0.8)
                .lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(14)
        .background(Color(UIColor.secondarySystemGroupedBackground))
        .cornerRadius(14)
        .shadow(color: Color.black.opacity(0.03), radius: 3, y: 1)
    }

    // MARK: - Node Status Card
    private var nodeStatusCard: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Label("Active SPP Node", systemImage: "network")
                    .font(.subheadline)
                    .fontWeight(.semibold)
                Spacer()
                Text(vpnManager.latencyText)
                    .font(.caption)
                    .fontWeight(.bold)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 3)
                    .background(Color.green.opacity(0.15))
                    .foregroundColor(.green)
                    .cornerRadius(8)
            }

            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text(vpnManager.activeNodeText)
                        .font(.body)
                        .fontWeight(.medium)
                    Text("Auto failover & latency heartbeat (15s)")
                        .font(.caption2)
                        .foregroundColor(.secondary)
                }
                Spacer()
            }
        }
        .padding(16)
        .background(Color(UIColor.secondarySystemGroupedBackground))
        .cornerRadius(16)
        .shadow(color: Color.black.opacity(0.04), radius: 5, y: 2)
    }

    // MARK: - Features Card
    private var featuresCard: some View {
        VStack(spacing: 12) {
            featureRow(icon: "bolt.shield.fill", title: "Fake-IP Mode", desc: "198.18.0.0/15 (0ms DNS Response)", badge: "Active", color: .indigo)
            Divider()
            featureRow(icon: "cpu", title: "User-space Stack", desc: "Pure Go gVisor netstack + tun2socks", badge: "v2.7", color: .teal)
            Divider()
            featureRow(icon: "lock.shield", title: "Tunnel Protocol", desc: "SPP over TCP/KCP/QUIC (Encrypted)", badge: "AES-GCM", color: .blue)
        }
        .padding(16)
        .background(Color(UIColor.secondarySystemGroupedBackground))
        .cornerRadius(16)
        .shadow(color: Color.black.opacity(0.04), radius: 5, y: 2)
    }

    private func featureRow(icon: String, title: String, desc: String, badge: String, color: Color) -> some View {
        HStack(spacing: 12) {
            Image(systemName: icon)
                .font(.system(size: 20))
                .foregroundColor(color)
                .frame(width: 28)

            VStack(alignment: .leading, spacing: 2) {
                Text(title)
                    .font(.subheadline)
                    .fontWeight(.semibold)
                Text(desc)
                    .font(.caption2)
                    .foregroundColor(.secondary)
            }

            Spacer()

            Text(badge)
                .font(.caption2)
                .fontWeight(.medium)
                .padding(.horizontal, 6)
                .padding(.vertical, 2)
                .background(Color.secondary.opacity(0.12))
                .cornerRadius(6)
        }
    }

    // MARK: - Actions
    private var actionButtons: some View {
        Button(action: {
            editingYAML = configStore.configYAML
            showingConfigEditor = true
        }) {
            HStack {
                Image(systemName: "pencil.and.outline")
                Text("Edit Configuration (YAML)")
            }
            .font(.subheadline)
            .fontWeight(.semibold)
            .foregroundColor(.accentColor)
            .frame(maxWidth: .infinity)
            .padding()
            .background(Color(UIColor.secondarySystemGroupedBackground))
            .cornerRadius(14)
        }
    }

    // MARK: - Config Editor Sheet
    private var configEditorSheet: some View {
        NavigationView {
            VStack(spacing: 0) {
                TextEditor(text: $editingYAML)
                    .font(.system(.body, design: .monospaced))
                    .padding(8)
            }
            .navigationTitle("Configuration")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("Reset") {
                        editingYAML = ConfigStore.defaultConfig
                    }
                    .foregroundColor(.red)
                }

                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        showingConfigEditor = false
                    }
                }

                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") {
                        configStore.saveConfig(editingYAML)
                        showingConfigEditor = false
                        if vpnManager.isConnected {
                            vpnManager.startVPN(configContent: editingYAML)
                        }
                    }
                    .fontWeight(.bold)
                }
            }
        }
    }
}
