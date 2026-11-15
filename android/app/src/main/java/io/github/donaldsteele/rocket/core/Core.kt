package io.github.donaldsteele.rocket.core

import android.content.Context
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.content.edit
import io.github.donaldsteele.rocket.BuildConfig
import io.github.donaldsteele.rocket.core.rocketcore.Node
import io.github.donaldsteele.rocket.core.rocketcore.Rocketcore
import io.github.donaldsteele.rocket.service.TransferService
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.File

/** The app's colour theme. Dark is the default, as on desktop. */
enum class ThemeMode { Dark, Light, System }

/**
 * The app-wide state: the Go node (identity, trust store and history in the
 * app's private files folder), discovery, the sends, and a few preferences.
 */
class Core(context: Context) {
    private val app = context.applicationContext
    private val prefs = app.getSharedPreferences("rocket", Context.MODE_PRIVATE)

    val node: Node = Rocketcore.newNode(
        File(app.filesDir, "rocket").path,
        prefs.getString(KEY_NAME, null) ?: defaultDeviceName(app),
        BuildConfig.VERSION_NAME,
    )

    val items = ContentItems(app.contentResolver)
    val discovery = Discovery(app, node)
    val transfers = Transfers(node, items)

    private val _theme = MutableStateFlow(
        runCatching { ThemeMode.valueOf(prefs.getString(KEY_THEME, null) ?: "") }.getOrDefault(ThemeMode.Dark),
    )
    val theme: StateFlow<ThemeMode> = _theme.asStateFlow()

    private val _name = MutableStateFlow(node.name())

    /** The device name others see. */
    val name: StateFlow<String> = _name.asStateFlow()

    val fingerprint: String get() = node.fingerprint()

    fun setTheme(mode: ThemeMode) {
        prefs.edit { putString(KEY_THEME, mode.name) }
        _theme.value = mode
    }

    /** Renames this device (the core cleans the name up; "" means the default). */
    fun setName(name: String) {
        node.setName(name.ifBlank { defaultDeviceName(app) })
        val cleaned = node.name()
        prefs.edit { putString(KEY_NAME, cleaned) }
        _name.value = cleaned
    }

    /** Whether the notification permission question was asked already. */
    var askedNotifications: Boolean
        get() = prefs.getBoolean(KEY_ASKED_NOTIFY, false)
        set(v) = prefs.edit { putBoolean(KEY_ASKED_NOTIFY, v) }

    /**
     * Sends items to target in the transfer service. grants are content URIs
     * this app may read only through a grant tied to the calling activity
     * (the share sheet's); they are handed on to the service so they stay
     * readable after the activity closes.
     */
    fun send(context: Context, target: SendTarget, items: List<SendItem>, grants: List<Uri> = emptyList()) {
        val job = transfers.enqueue(target, items)
        try {
            TransferService.startSend(context, job, grants)
        } catch (e: Exception) {
            transfers.forget(job)
            throw e
        }
    }

    private companion object {
        const val KEY_NAME = "device_name"
        const val KEY_THEME = "theme"
        const val KEY_ASKED_NOTIFY = "asked_notifications"

        fun defaultDeviceName(ctx: Context): String =
            Settings.Global.getString(ctx.contentResolver, Settings.Global.DEVICE_NAME)?.takeIf { it.isNotBlank() }
                ?: Build.MODEL
    }
}
