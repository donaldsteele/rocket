package io.github.donaldsteele.rocket.core

import android.content.Context
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.content.edit
import io.github.donaldsteele.rocket.BuildConfig
import io.github.donaldsteele.rocket.core.rocketcore.Node
import io.github.donaldsteele.rocket.core.rocketcore.Rocketcore
import io.github.donaldsteele.rocket.service.ReceiveService
import io.github.donaldsteele.rocket.service.TransferService
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.File

/** The app's colour theme. Dark is the default, as on desktop. */
enum class ThemeMode { Dark, Light, System }

/**
 * The app-wide state: the Go node (identity, trust store and history in the
 * app's private files folder), discovery, the receiver, the transfers, and
 * a few preferences.
 */
class Core(context: Context) {
    private val app = context.applicationContext
    private val prefs = app.getSharedPreferences("rocket", Context.MODE_PRIVATE)

    val node: Node = Rocketcore.newNode(
        File(app.filesDir, "rocket").path,
        prefs.getString(KEY_NAME, null) ?: defaultDeviceName(app),
        BuildConfig.VERSION_NAME,
    )

    val items = ContentItems(app)
    val transfers = Transfers(node, items, OutgoingStore(File(app.filesDir, "rocket/outgoing-items"), items))
    val receiver = Receiver(app, node, transfers)
    val discovery = Discovery(app, node, ownService = { receiver.serviceName })

    private val _theme = MutableStateFlow(
        runCatching { ThemeMode.valueOf(prefs.getString(KEY_THEME, null) ?: "") }.getOrDefault(ThemeMode.Dark),
    )
    val theme: StateFlow<ThemeMode> = _theme.asStateFlow()

    private val _name = MutableStateFlow(node.name())

    /** The device name others see. */
    val name: StateFlow<String> = _name.asStateFlow()

    val fingerprint: String get() = node.fingerprint()

    private val _trusted = MutableStateFlow<List<TrustedSender>>(emptyList())

    /** Senders whose key is pinned, most recently approved first ([reloadTrusted]). */
    val trusted: StateFlow<List<TrustedSender>> = _trusted.asStateFlow()

    private val _autoAccept = MutableStateFlow(node.autoAccept())

    /** Whether trusted senders skip the prompt. */
    val autoAccept: StateFlow<Boolean> = _autoAccept.asStateFlow()

    fun reloadTrusted() {
        _trusted.value = runCatching { parseTrusted(node.trustedJSON()) }.getOrDefault(emptyList())
    }

    /** Forgets trusted sender fp: its next offer asks again. */
    fun revoke(fp: String) {
        node.revoke(fp)
        reloadTrusted()
    }

    fun setAutoAccept(on: Boolean) {
        node.setAutoAccept(on)
        _autoAccept.value = node.autoAccept()
    }

    /**
     * Whether the person has receiving on. Remembered, so the receiver
     * comes back when the app is opened after the system stopped it.
     */
    var receiveWanted: Boolean
        get() = prefs.getBoolean(KEY_RECEIVE, false)
        set(v) = prefs.edit { putBoolean(KEY_RECEIVE, v) }

    /** Turns receiving on (call from a visible activity). */
    fun startReceiving(context: Context) {
        receiveWanted = true
        ReceiveService.start(context)
    }

    /** Turns receiving off; the receive service stops itself. */
    fun stopReceiving() {
        receiveWanted = false
        receiver.stop()
    }

    fun setTheme(mode: ThemeMode) {
        prefs.edit { putString(KEY_THEME, mode.name) }
        _theme.value = mode
    }

    /**
     * Renames this device (the core cleans the name up; "" means the
     * default). A running receiver restarts to announce it, unless that
     * would cut off an offer or a receive; returns false in that case.
     */
    fun setName(name: String): Boolean {
        node.setName(name.ifBlank { defaultDeviceName(app) })
        val cleaned = node.name()
        prefs.edit { putString(KEY_NAME, cleaned) }
        _name.value = cleaned
        return receiver.state.value != ReceiverState.On || receiver.restartIfIdle()
    }

    /** Whether the notification permission question was asked already. */
    var askedNotifications: Boolean
        get() = prefs.getBoolean(KEY_ASKED_NOTIFY, false)
        set(v) = prefs.edit { putBoolean(KEY_ASKED_NOTIFY, v) }

    init {
        transfers.reloadResumable()
    }

    /**
     * Sends items to target in the transfer service. grants are content URIs
     * this app may read only through a grant tied to the calling activity
     * (the share sheet's); they are handed on to the service so they stay
     * readable after the activity closes. kept are the URIs this app took
     * persistable access to (ContentItems.take), for a Resume later.
     */
    fun send(
        context: Context,
        target: SendTarget,
        items: List<SendItem>,
        grants: List<Uri> = emptyList(),
        kept: List<String> = emptyList(),
    ) {
        val job = transfers.enqueue(target, items, kept)
        try {
            TransferService.startSend(context, job, grants)
        } catch (e: Exception) {
            transfers.forget(job)
            throw e
        }
    }

    /**
     * Resumes interrupted send [send] with items (the saved ones, or files
     * picked again) in the transfer service: to the same peer, continuing
     * where it stopped when the files are unchanged.
     */
    fun resume(context: Context, send: InterruptedSend, items: List<SendItem>, kept: List<String>) {
        val target = SendTarget(addr = "", fp = send.peerFp, name = send.peerName)
        val job = transfers.enqueue(target, items, kept, resumeOf = send.transferId)
        try {
            TransferService.startSend(context, job, emptyList())
        } catch (e: Exception) {
            transfers.forget(job)
            throw e
        }
    }

    private companion object {
        const val KEY_NAME = "device_name"
        const val KEY_THEME = "theme"
        const val KEY_ASKED_NOTIFY = "asked_notifications"
        const val KEY_RECEIVE = "receive_on"

        fun defaultDeviceName(ctx: Context): String =
            Settings.Global.getString(ctx.contentResolver, Settings.Global.DEVICE_NAME)?.takeIf { it.isNotBlank() }
                ?: Build.MODEL
    }
}
