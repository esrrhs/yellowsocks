package com.esrrhs.yellowsocks

import android.app.Activity
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.delay
import mobile.Mobile

data class AppItem(val name: String, val packageName: String, var isBypassed: Boolean)

class MainActivity : ComponentActivity() {

    private var isProxyRunning by mutableStateOf(false)
    private var configText by mutableStateOf(
"""# YellowSocks Android Configuration
spp_server: "1.2.3.4:8888"
spp_proto: "tcp"
spp_key: "123456"
fake_ip: true
direct_dns: "1.1.1.1:53"
doh_url: "https://1.1.1.1/dns-query"
"""
    )
    private var installedApps = mutableStateListOf<AppItem>()
    private var uploadSpeedText by mutableStateOf("0 B/s")
    private var downloadSpeedText by mutableStateOf("0 B/s")

    private val vpnPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult()
    ) { result ->
        if (result.resultCode == Activity.RESULT_OK) {
            launchVpnService()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        loadInstalledApps()

        setContent {
            MaterialTheme {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background
                ) {
                    MainScreen()
                }
            }
        }
    }

    private fun loadInstalledApps() {
        val pm = packageManager
        val apps = pm.getInstalledApplications(PackageManager.GET_META_DATA)
        val list = mutableListOf<AppItem>()
        for (app in apps) {
            if ((app.flags and ApplicationInfo.FLAG_SYSTEM) == 0 && app.packageName != packageName) {
                val label = pm.getApplicationLabel(app).toString()
                list.add(AppItem(label, app.packageName, false))
            }
        }
        installedApps.clear()
        installedApps.addAll(list.sortedBy { it.name })
    }

    private fun startVpn() {
        val vpnIntent = VpnService.prepare(this)
        if (vpnIntent != null) {
            vpnPermissionLauncher.launch(vpnIntent)
        } else {
            launchVpnService()
        }
    }

    private fun launchVpnService() {
        val intent = Intent(this, YellowSocksVpnService::class.java).apply {
            action = YellowSocksVpnService.ACTION_START
            putExtra(YellowSocksVpnService.EXTRA_CONFIG, configText)
            val bypassed = ArrayList(installedApps.filter { it.isBypassed }.map { it.packageName })
            putStringArrayListExtra(YellowSocksVpnService.EXTRA_BYPASS_PKGS, bypassed)
        }
        startService(intent)
        isProxyRunning = true
    }

    private fun stopVpn() {
        val intent = Intent(this, YellowSocksVpnService::class.java).apply {
            action = YellowSocksVpnService.ACTION_STOP
        }
        startService(intent)
        isProxyRunning = false
    }

    @OptIn(ExperimentalMaterial3Api::class)
    @Composable
    fun MainScreen() {
        var selectedTab by remember { mutableStateOf(0) }

        LaunchedEffect(Unit) {
            while (true) {
                isProxyRunning = Mobile.isRunning()
                if (isProxyRunning) {
                    uploadSpeedText = formatBytes(YellowSocksVpnService.lastUploadBps)
                    downloadSpeedText = formatBytes(YellowSocksVpnService.lastDownloadBps)
                }
                delay(1000)
            }
        }

        Scaffold(
            topBar = {
                TopAppBar(
                    title = { Text("YellowSocks Android", fontWeight = FontWeight.Bold) },
                    colors = TopAppBarDefaults.topAppBarColors(
                        containerColor = MaterialTheme.colorScheme.primaryContainer
                    )
                )
            },
            bottomBar = {
                NavigationBar {
                    NavigationBarItem(
                        selected = selectedTab == 0,
                        onClick = { selectedTab = 0 },
                        label = { Text("Dashboard") },
                        icon = { Text("⚡", fontSize = 18.sp) }
                    )
                    NavigationBarItem(
                        selected = selectedTab == 1,
                        onClick = { selectedTab = 1 },
                        label = { Text("Config") },
                        icon = { Text("⚙️", fontSize = 18.sp) }
                    )
                    NavigationBarItem(
                        selected = selectedTab == 2,
                        onClick = { selectedTab = 2 },
                        label = { Text("Apps Bypass") },
                        icon = { Text("📱", fontSize = 18.sp) }
                    )
                }
            }
        ) { padding ->
            Box(modifier = Modifier.padding(padding)) {
                when (selectedTab) {
                    0 -> DashboardTab()
                    1 -> ConfigTab()
                    2 -> AppsBypassTab()
                }
            }
        }
    }

    @Composable
    fun DashboardTab() {
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(24.dp)
        ) {
            Spacer(modifier = Modifier.height(16.dp))

            // 主连接切换大按钮
            Box(
                modifier = Modifier
                    .size(160.dp)
                    .background(
                        color = if (isProxyRunning) Color(0xFF2E7D32) else Color(0xFF757575),
                        shape = CircleShape
                    )
                    .clickable {
                        if (isProxyRunning) stopVpn() else startVpn()
                    },
                contentAlignment = Alignment.Center
            ) {
                Column(horizontalAlignment = Alignment.CenterHorizontally) {
                    Text(
                        text = if (isProxyRunning) "CONNECTED" else "STOPPED",
                        color = Color.White,
                        fontWeight = FontWeight.Bold,
                        fontSize = 18.sp
                    )
                    Text(
                        text = if (isProxyRunning) "Tap to Stop" else "Tap to Start",
                        color = Color.White.copy(alpha = 0.8f),
                        fontSize = 12.sp
                    )
                }
            }

            // 实时速率卡片
            Card(
                modifier = Modifier.fillMaxWidth(),
                shape = RoundedCornerShape(16.dp),
                colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceVariant)
            ) {
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(20.dp),
                    horizontalArrangement = Arrangement.SpaceAround
                ) {
                    Column(horizontalAlignment = Alignment.CenterHorizontally) {
                        Text("↑ Upload", fontSize = 14.sp, color = MaterialTheme.colorScheme.outline)
                        Text(uploadSpeedText, fontSize = 20.sp, fontWeight = FontWeight.Bold)
                    }
                    Column(horizontalAlignment = Alignment.CenterHorizontally) {
                        Text("↓ Download", fontSize = 14.sp, color = MaterialTheme.colorScheme.outline)
                        Text(downloadSpeedText, fontSize = 20.sp, fontWeight = FontWeight.Bold)
                    }
                }
            }

            // 当前节点与功能卡片
            Card(
                modifier = Modifier.fillMaxWidth(),
                shape = RoundedCornerShape(16.dp)
            ) {
                Column(modifier = Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text("Routing & Proxy Engine", fontWeight = FontWeight.Bold)
                    Text("• TUN Provider: Android VpnService", fontSize = 13.sp)
                    Text("• TCP + UDP Forwarding via SPP", fontSize = 13.sp)
                    Text("• Fake-IP Mode: 0ms DNS Resolution", fontSize = 13.sp)
                    Text("• Upstream: SPP Protocol Tunnel", fontSize = 13.sp)
                    Text("• Active Node: ${YellowSocksVpnService.activeNodeInfo}", fontSize = 13.sp)
                }
            }
        }
    }

    @Composable
    fun ConfigTab() {
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp)
        ) {
            Text("YAML / JSON Configuration", fontWeight = FontWeight.Bold)
            OutlinedTextField(
                value = configText,
                onValueChange = { configText = it },
                modifier = Modifier
                    .fillMaxWidth()
                    .weight(1f),
                enabled = !isProxyRunning,
                placeholder = { Text("Paste your YAML configuration here...") }
            )
            if (isProxyRunning) {
                Text("⚠️ Stop proxy before editing configuration", color = Color(0xFFC62828), fontSize = 13.sp)
            }
        }
    }

    @Composable
    fun AppsBypassTab() {
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(16.dp)
        ) {
            Text(
                "Select Applications to Bypass (Direct Route)",
                fontWeight = FontWeight.Bold,
                modifier = Modifier.padding(bottom = 12.dp)
            )
            LazyColumn(
                modifier = Modifier.fillMaxSize(),
                verticalArrangement = Arrangement.spacedBy(4.dp)
            ) {
                items(installedApps) { app ->
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .clickable {
                                if (!isProxyRunning) {
                                    app.isBypassed = !app.isBypassed
                                }
                            }
                            .padding(vertical = 8.dp, horizontal = 4.dp),
                        verticalAlignment = Alignment.CenterVertically
                    ) {
                        Column(modifier = Modifier.weight(1f)) {
                            Text(app.name, fontWeight = FontWeight.Medium)
                            Text(app.packageName, fontSize = 12.sp, color = MaterialTheme.colorScheme.outline)
                        }
                        Checkbox(
                            checked = app.isBypassed,
                            onCheckedChange = { checked ->
                                if (!isProxyRunning) {
                                    app.isBypassed = checked
                                }
                            },
                            enabled = !isProxyRunning
                        )
                    }
                    Divider(color = MaterialTheme.colorScheme.outlineVariant.copy(alpha = 0.5f))
                }
            }
        }
    }

    private fun formatBytes(bytes: Long): String {
        return when {
            bytes >= 1024 * 1024 -> String.format("%.1f MB/s", bytes / (1024.0 * 1024.0))
            bytes >= 1024 -> String.format("%.1f KB/s", bytes / 1024.0)
            else -> "$bytes B/s"
        }
    }
}
