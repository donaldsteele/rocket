package io.github.donaldsteele.rocket.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat
import io.github.donaldsteele.rocket.core

/**
 * Returns a function that asks once (Android 13+) for permission to show
 * notifications, so transfer progress can show outside the app. Sending
 * works either way.
 */
@Composable
fun rememberNotificationAsk(): () -> Unit {
    val context = LocalContext.current
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { }
    return ask@{
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return@ask
        val core = context.core
        if (core.askedNotifications) return@ask
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) return@ask
        core.askedNotifications = true
        launcher.launch(Manifest.permission.POST_NOTIFICATIONS)
    }
}
