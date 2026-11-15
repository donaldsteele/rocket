package io.github.donaldsteele.rocket.service

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import io.github.donaldsteele.rocket.R
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.IncomingOffer
import io.github.donaldsteele.rocket.core.Transfer
import io.github.donaldsteele.rocket.ui.MainActivity
import io.github.donaldsteele.rocket.ui.countWhat
import io.github.donaldsteele.rocket.ui.formatSize
import io.github.donaldsteele.rocket.ui.namesLine
import io.github.donaldsteele.rocket.ui.offerWhat

/**
 * Notifications of the receiving side: a high-priority one per incoming
 * offer, with Accept and Decline, and one when a receive has finished.
 * "Always accept" is only offered in the app, next to the fingerprint.
 */
object OfferNotifications {
    const val CHANNEL_OFFERS = "offers"
    const val CHANNEL_RECEIVED = "received"

    /** How long an offer waits for an answer (protocol.OfferWait). */
    private const val OFFER_WAIT_MS = 2 * 60 * 1000L

    fun ensureChannels(context: Context) {
        val nm = context.getSystemService(NotificationManager::class.java) ?: return
        val offers = NotificationChannel(CHANNEL_OFFERS, context.getString(R.string.channel_offers), NotificationManager.IMPORTANCE_HIGH)
        offers.description = context.getString(R.string.channel_offers_desc)
        val received = NotificationChannel(CHANNEL_RECEIVED, context.getString(R.string.channel_received), NotificationManager.IMPORTANCE_DEFAULT)
        received.description = context.getString(R.string.channel_received_desc)
        nm.createNotificationChannels(listOf(offers, received))
    }

    /** Whether notifications can be shown at all (Android 13+ asks). */
    fun allowed(context: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED

    fun showOffer(context: Context, offer: IncomingOffer) {
        ensureChannels(context)
        val what = offerWhat(offer)
        val size = if (offer.size > 0) " (${formatSize(offer.size)})" else ""
        val lines = buildList {
            if (offer.files > 1 && offer.names.isNotEmpty()) add(namesLine(offer))
            if (offer.senderFp.isNotEmpty()) add("Fingerprint ${offer.senderFp}")
        }
        val b = NotificationCompat.Builder(context, CHANNEL_OFFERS)
            .setSmallIcon(R.drawable.ic_stat_rocket)
            .setContentTitle("${offer.senderName} wants to send you $what$size")
            .setContentText(lines.firstOrNull() ?: "Accept or decline")
            .setStyle(NotificationCompat.BigTextStyle().bigText(lines.joinToString("\n").ifEmpty { "Accept or decline" }))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setContentIntent(openApp(context, offer.id))
            .setAutoCancel(false)
            .setOngoing(true)
            .setTimeoutAfter(OFFER_WAIT_MS)
            .addAction(0, "Accept", answer(context, offer.id, OfferActionReceiver.ACTION_ACCEPT))
            .addAction(0, "Decline", answer(context, offer.id, OfferActionReceiver.ACTION_DECLINE))
        post(context, offerNotificationId(offer.id), b.build())
    }

    fun cancelOffer(context: Context, id: String) {
        context.getSystemService(NotificationManager::class.java)?.cancel(offerNotificationId(id))
    }

    fun showReceived(context: Context, t: Transfer, where: String) {
        ensureChannels(context)
        val open = PendingIntent.getActivity(
            context, 2,
            Intent(context, MainActivity::class.java)
                .putExtra(MainActivity.EXTRA_TAB, MainActivity.TAB_TRANSFERS)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val b = NotificationCompat.Builder(context, CHANNEL_RECEIVED)
            .setSmallIcon(R.drawable.ic_stat_rocket)
            .setContentTitle("Received ${countWhat(t.name, t.files)} from ${t.peerName}")
            .setContentText("Saved in $where")
            .setCategory(NotificationCompat.CATEGORY_STATUS)
            .setContentIntent(open)
            .setAutoCancel(true)
        post(context, receivedNotificationId(t.id), b.build())
    }

    private fun post(context: Context, id: Int, n: Notification) {
        // Without the permission offers still show in the app.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            return
        }
        context.getSystemService(NotificationManager::class.java)?.notify(id, n)
    }

    private fun openApp(context: Context, offerId: String): PendingIntent = PendingIntent.getActivity(
        context, 0,
        Intent(context, MainActivity::class.java)
            .setData(offerUri(offerId))
            .putExtra(MainActivity.EXTRA_TAB, MainActivity.TAB_RECEIVE)
            .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    private fun answer(context: Context, offerId: String, action: String): PendingIntent = PendingIntent.getBroadcast(
        context, 0,
        Intent(context, OfferActionReceiver::class.java)
            .setAction(action)
            .setData(offerUri(offerId))
            .putExtra(OfferActionReceiver.EXTRA_OFFER, offerId),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    /** Makes each offer's intents distinct (extras do not count). */
    private fun offerUri(id: String): Uri = Uri.Builder().scheme("rocket").authority("offer").appendPath(id).build()

    private fun offerNotificationId(id: String) = 1000 + (id.hashCode() and 0x3fffffff) % 1_000_000

    private fun receivedNotificationId(id: String) = 2_000_000 + (id.hashCode() and 0x3fffffff) % 1_000_000
}

/** Accept or Decline pressed on an offer notification. */
class OfferActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.getStringExtra(EXTRA_OFFER) ?: return
        val accept = when (intent.action) {
            ACTION_ACCEPT -> true
            ACTION_DECLINE -> false
            else -> return
        }
        context.core.receiver.answer(id, accept, trust = false)
        OfferNotifications.cancelOffer(context, id)
    }

    companion object {
        const val ACTION_ACCEPT = "io.github.donaldsteele.rocket.action.ACCEPT_OFFER"
        const val ACTION_DECLINE = "io.github.donaldsteele.rocket.action.DECLINE_OFFER"
        const val EXTRA_OFFER = "offer"
    }
}
