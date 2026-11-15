package io.github.donaldsteele.rocket.service

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import io.github.donaldsteele.rocket.R
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.ReceiverState
import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.ui.MainActivity
import io.github.donaldsteele.rocket.ui.countWhat
import io.github.donaldsteele.rocket.ui.formatSize
import kotlinx.coroutines.MainScope
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.conflate
import kotlinx.coroutines.launch

/**
 * The foreground service that keeps the receiver running while receiving
 * is on, also with the app in the background, with an ongoing notification
 * that says so (and shows receives in progress). It is a connectedDevice
 * service: it serves other devices on the local network, and unlike
 * dataSync it has no daily time limit. It stops when receiving is turned off.
 */
class ReceiveService : Service() {
    private val scope = MainScope()
    private var foreground = false

    /** Set once a start command was handled; before that "off" is just the initial state. */
    private var started = false

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        ensureChannel(this)
        OfferNotifications.ensureChannels(this)
        val c = core
        scope.launch {
            combine(c.receiver.state, c.receiver.port, c.transfers.list, c.name) { state, port, list, name ->
                Snapshot(state, port, list.filter { it.incoming && it.state.active }, name)
            }
                .conflate()
                .collect { s ->
                    if (s.state == ReceiverState.Off) {
                        if (!started) return@collect
                        if (foreground) {
                            ServiceCompat.stopForeground(this@ReceiveService, ServiceCompat.STOP_FOREGROUND_REMOVE)
                            foreground = false
                        }
                        stopSelf()
                        return@collect
                    }
                    notifyStatus(s)
                    delay(500)
                }
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val c = core
        when (intent?.action) {
            // "Turn off" on the notification: no new foreground start needed.
            ACTION_STOP -> {
                c.receiveWanted = false
                c.receiver.stop()
            }
            else -> {
                goForeground()
                c.receiver.start()
            }
        }
        started = true
        if (c.receiver.state.value == ReceiverState.Off) {
            // Nothing to stop (receiving was already off).
            if (foreground) {
                ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE)
                foreground = false
            }
            stopSelf()
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    private data class Snapshot(val state: ReceiverState, val port: Int, val active: List<Transfer>, val name: String)

    private fun goForeground() {
        val r = core.receiver
        val s = Snapshot(r.state.value, r.port.value, emptyList(), core.name.value)
        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE else 0
        ServiceCompat.startForeground(this, NOTIFICATION_ID, build(s), type)
        foreground = true
    }

    private fun notifyStatus(s: Snapshot) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            return
        }
        getSystemService(NotificationManager::class.java)?.notify(NOTIFICATION_ID, build(s))
    }

    private fun build(s: Snapshot): Notification {
        val open = PendingIntent.getActivity(
            this, 3,
            Intent(this, MainActivity::class.java)
                .putExtra(MainActivity.EXTRA_TAB, MainActivity.TAB_RECEIVE)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this, 4,
            Intent(this, ReceiveService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val sent = s.active.sumOf { it.sent }
        val total = s.active.sumOf { it.total }
        val title = when {
            s.active.size == 1 -> "Receiving ${countWhat(s.active[0].name, s.active[0].files)} from ${s.active[0].peerName}"
            s.active.size > 1 -> "Receiving ${s.active.size} transfers"
            s.state == ReceiverState.On -> "Receiving is on"
            s.state == ReceiverState.Stopping -> "Turning receiving off"
            else -> "Turning receiving on"
        }
        val text = when {
            s.active.isNotEmpty() -> "${formatSize(sent)} of ${formatSize(total)}"
            else -> "Visible as ${s.name} on this network"
        }
        val b = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_rocket)
            .setContentTitle(title)
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .addAction(0, "Turn off", stop)
        if (s.active.isNotEmpty()) {
            if (total > 0) {
                b.setProgress(PROGRESS_MAX, (sent * PROGRESS_MAX / total).toInt().coerceIn(0, PROGRESS_MAX), false)
            } else {
                b.setProgress(0, 0, true)
            }
        }
        return b.build()
    }

    companion object {
        private const val ACTION_STOP = "io.github.donaldsteele.rocket.action.STOP_RECEIVING"
        private const val CHANNEL_ID = "receiver"
        private const val NOTIFICATION_ID = 2
        private const val PROGRESS_MAX = 1000

        /** Turns receiving on (from the app, while it is in the foreground). */
        fun start(context: Context) {
            ContextCompat.startForegroundService(context, Intent(context, ReceiveService::class.java))
        }

        private fun ensureChannel(context: Context) {
            val nm = context.getSystemService(NotificationManager::class.java) ?: return
            val ch = NotificationChannel(CHANNEL_ID, context.getString(R.string.channel_receiver), NotificationManager.IMPORTANCE_LOW)
            ch.description = context.getString(R.string.channel_receiver_desc)
            nm.createNotificationChannel(ch)
        }
    }
}
