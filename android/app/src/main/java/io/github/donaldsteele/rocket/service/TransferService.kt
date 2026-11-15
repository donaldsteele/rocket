package io.github.donaldsteele.rocket.service

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.ClipData
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.Uri
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import io.github.donaldsteele.rocket.R
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.core.TransferState
import io.github.donaldsteele.rocket.ui.MainActivity
import io.github.donaldsteele.rocket.ui.formatSize
import kotlinx.coroutines.MainScope
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.conflate
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

/**
 * A foreground service that keeps sends running while the app is in the
 * background, with a notification showing their progress. It runs while
 * any send is queued or in flight and stops itself afterwards.
 */
class TransferService : Service() {
    private val scope = MainScope()
    private var lastStartId = 0
    private var foreground = false

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        ensureChannel(this)
        val transfers = core.transfers
        scope.launch {
            // Conflated, so while one update waits out its pause only the
            // newest state is kept: at most two notification updates a second.
            combine(transfers.list, transfers.starting) { list, starting -> list to starting }
                .conflate()
                .collect { (list, starting) ->
                    val active = list.filter { it.state.active && !it.incoming }
                    if (active.isEmpty() && starting == 0) {
                        if (foreground) {
                            ServiceCompat.stopForeground(this@TransferService, ServiceCompat.STOP_FOREGROUND_REMOVE)
                            foreground = false
                        }
                        // Only stops when no newer start is pending, so a send
                        // queued just now still gets its foreground service.
                        stopSelfResult(lastStartId)
                        return@collect
                    }
                    notifyProgress(active, starting)
                    delay(500)
                }
        }
    }

    // Android 15 limits dataSync foreground services to 6 hours a day. When
    // the limit is reached, running sends are cancelled and the service
    // stops, as the system requires.
    override fun onTimeout(startId: Int, fgsType: Int) {
        core.transfers.cancelAll()
        ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE)
        foreground = false
        stopSelf()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        lastStartId = startId
        goForeground()
        when (intent?.action) {
            ACTION_SEND -> core.transfers.start(intent.getLongExtra(EXTRA_JOB, -1))
            ACTION_CANCEL_ALL -> core.transfers.cancelAll()
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    private fun goForeground() {
        val list = core.transfers.list.value.filter { it.state.active && !it.incoming }
        val type = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC else 0
        ServiceCompat.startForeground(this, NOTIFICATION_ID, build(list, core.transfers.starting.value), type)
        foreground = true
    }

    private fun notifyProgress(active: List<Transfer>, starting: Int) {
        // Without the permission the service still runs; the notification
        // is just not shown.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            return
        }
        getSystemService(NotificationManager::class.java)?.notify(NOTIFICATION_ID, build(active, starting))
    }

    private fun build(active: List<Transfer>, starting: Int): android.app.Notification {
        val open = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java)
                .putExtra(MainActivity.EXTRA_TAB, MainActivity.TAB_TRANSFERS)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val cancel = PendingIntent.getService(
            this, 1,
            Intent(this, TransferService::class.java).setAction(ACTION_CANCEL_ALL),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val sent = active.sumOf { it.sent }
        val total = active.sumOf { it.total }
        val title = when {
            active.size == 1 -> "Sending ${active[0].name} to ${active[0].peerName}"
            active.size > 1 -> "Sending ${active.size} transfers"
            else -> "Preparing to send"
        }
        val text = when {
            active.any { it.state == TransferState.Waiting } && sent == 0L -> "Waiting for the receiver to accept"
            total > 0 -> "${formatSize(sent)} of ${formatSize(total)}"
            starting > 0 -> "Reading the files"
            else -> ""
        }
        val b = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_rocket)
            .setContentTitle(title)
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .setCategory(NotificationCompat.CATEGORY_PROGRESS)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
        if (total > 0 && sent > 0) {
            b.setProgress(PROGRESS_MAX, (sent * PROGRESS_MAX / total).toInt().coerceIn(0, PROGRESS_MAX), false)
        } else {
            b.setProgress(0, 0, true)
        }
        if (active.isNotEmpty()) b.addAction(0, "Cancel", cancel)
        return b.build()
    }

    companion object {
        private const val ACTION_SEND = "io.github.donaldsteele.rocket.action.SEND"
        private const val ACTION_CANCEL_ALL = "io.github.donaldsteele.rocket.action.CANCEL_ALL"
        private const val EXTRA_JOB = "job"
        private const val CHANNEL_ID = "transfers"
        private const val NOTIFICATION_ID = 1
        private const val PROGRESS_MAX = 1000

        /**
         * Starts queued send [job] in the service. The URIs in grants are
         * attached with a read grant, which then lasts as long as the
         * service runs.
         */
        fun startSend(context: Context, job: Long, grants: List<Uri>) {
            val intent = Intent(context, TransferService::class.java)
                .setAction(ACTION_SEND)
                .putExtra(EXTRA_JOB, job)
            if (grants.isNotEmpty()) {
                val clip = ClipData.newRawUri("", grants[0])
                grants.drop(1).forEach { clip.addItem(ClipData.Item(it)) }
                intent.clipData = clip
                intent.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            ContextCompat.startForegroundService(context, intent)
        }

        private fun ensureChannel(context: Context) {
            val nm = context.getSystemService(NotificationManager::class.java) ?: return
            val ch = NotificationChannel(CHANNEL_ID, context.getString(R.string.channel_transfers), NotificationManager.IMPORTANCE_LOW)
            ch.description = context.getString(R.string.channel_transfers_desc)
            nm.createNotificationChannel(ch)
        }
    }
}
