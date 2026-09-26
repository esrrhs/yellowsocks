package com.esrrhs.yellowsocks

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import androidx.core.app.NotificationCompat
import mobile.Callback
import mobile.Mobile
import java.io.IOException

class YellowSocksVpnService : VpnService() {

    private var vpnInterface: ParcelFileDescriptor? = null
    private var isRunning = false

    companion object {
        const val ACTION_START = "com.esrrhs.yellowsocks.START"
        const val ACTION_STOP = "com.esrrhs.yellowsocks.STOP"
        const val EXTRA_CONFIG = "extra_config"
        const val EXTRA_BYPASS_PKGS = "extra_bypass_pkgs"
        const val CHANNEL_ID = "yellowsocks_vpn_channel"
        const val NOTIFICATION_ID = 1001

        var currentService: YellowSocksVpnService? = null
        var lastUploadBps: Long = 0
        var lastDownloadBps: Long = 0
        var activeNodeInfo: String = "Connecting..."
    }

    override fun onCreate() {
        super.onCreate()
        currentService = this
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_START -> {
                val config = intent.getStringExtra(EXTRA_CONFIG) ?: ""
                val bypassPkgs = intent.getStringArrayListExtra(EXTRA_BYPASS_PKGS) ?: arrayListOf()
                startVpn(config, bypassPkgs)
            }
            ACTION_STOP -> {
                stopVpn()
            }
        }
        return START_NOT_STICKY
    }

    private fun startVpn(configContent: String, bypassPkgs: List<String>) {
        if (isRunning) return

        val notification = buildNotification("YellowSocks Connecting...", "Initializing TUN interface")
        startForeground(NOTIFICATION_ID, notification)

        try {
            val builder = Builder()
                .setSession("YellowSocks")
                .setMtu(1500)
                .addAddress("10.255.0.2", 24)
                .addDnsServer("10.255.0.1")
                .addRoute("0.0.0.0", 0) // 接管全局流量

            // 进程/应用分流：排除勾选的应用，直连免入 TUN
            for (pkg in bypassPkgs) {
                try {
                    builder.addDisallowedApplication(pkg)
                } catch (e: Exception) {
                    // 忽略不存在或已卸载的包
                }
            }

            // 保护本应用自身避免回环
            try {
                builder.addDisallowedApplication(packageName)
            } catch (_: Exception) {}

            vpnInterface = builder.establish()
            if (vpnInterface == null) {
                stopVpn()
                return
            }

            val tunFd = vpnInterface!!.fd

            // 启动 Go 底层内核
            Mobile.startEngine(tunFd.toLong(), configContent, object : Callback {
                override fun onStatusChanged(running: Boolean, message: String) {
                    isRunning = running
                    updateNotification("YellowSocks: $message")
                }

                override fun onBandwidthUpdate(uploadBps: Long, downloadBps: Long, totalUpload: Long, totalDownload: Long) {
                    lastUploadBps = uploadBps
                    lastDownloadBps = downloadBps
                    val upStr = formatSpeed(uploadBps)
                    val downStr = formatSpeed(downloadBps)
                    updateNotification("↑ $upStr  ↓ $downStr  |  $activeNodeInfo")
                }

                override fun onActiveNodeChanged(name: String, server: String, proto: String, latencyMs: Long) {
                    activeNodeInfo = "$name (${latencyMs}ms)"
                }

                override fun protect(fd: Int): Boolean {
                    // Bypass VPN for Direct UDP/TCP and SPP relay sockets
                    return this@YellowSocksVpnService.protect(fd)
                }
            })

            isRunning = true

        } catch (e: Exception) {
            e.printStackTrace()
            stopVpn()
        }
    }

    private fun stopVpn() {
        isRunning = false
        try {
            Mobile.stopEngine()
        } catch (_: Exception) {}

        try {
            vpnInterface?.close()
            vpnInterface = null
        } catch (_: IOException) {}

        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        currentService = null
        stopVpn()
        super.onDestroy()
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "YellowSocks Service",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Shows live YellowSocks connection state and bandwidth"
            }
            val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildNotification(title: String, text: String): Notification {
        val intent = Intent(this, MainActivity::class.java)
        val pendingIntent = PendingIntent.getActivity(
            this, 0, intent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(title)
            .setContentText(text)
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentIntent(pendingIntent)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(text: String) {
        if (!isRunning) return
        val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        manager.notify(NOTIFICATION_ID, buildNotification("YellowSocks Active", text))
    }

    private fun formatSpeed(bytesPerSec: Long): String {
        return when {
            bytesPerSec >= 1024 * 1024 -> String.format("%.1f MB/s", bytesPerSec / (1024.0 * 1024.0))
            bytesPerSec >= 1024 -> String.format("%.1f KB/s", bytesPerSec / 1024.0)
            else -> "$bytesPerSec B/s"
        }
    }
}
