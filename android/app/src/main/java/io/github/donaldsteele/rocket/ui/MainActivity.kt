package io.github.donaldsteele.rocket.ui

import android.content.Intent
import android.graphics.Color
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.NavigationBarItemDefaults
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.ui.theme.Rocket
import io.github.donaldsteele.rocket.ui.theme.RocketTheme
import io.github.donaldsteele.rocket.ui.theme.isDark
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    private var requestedTab by mutableIntStateOf(-1)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        requestedTab = intent.getIntExtra(EXTRA_TAB, -1)
        setContent {
            val mode by core.theme.collectAsStateWithLifecycle()
            RocketTheme(mode) {
                SystemBars(mode.isDark())
                AppShell(requestedTab, onTabShown = { requestedTab = -1 })
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        requestedTab = intent.getIntExtra(EXTRA_TAB, -1)
    }

    companion object {
        /** Opens the given tab ([TAB_TRANSFERS], ...). */
        const val EXTRA_TAB = "tab"
        const val TAB_SEND = 0
        const val TAB_TRANSFERS = 1
        const val TAB_HISTORY = 2
        const val TAB_SETTINGS = 3
    }
}

/** Light or dark status and navigation bar icons to match the theme. */
@Composable
fun ComponentActivity.SystemBars(dark: Boolean) {
    DisposableEffect(dark) {
        val style = if (dark) {
            SystemBarStyle.dark(Color.TRANSPARENT)
        } else {
            SystemBarStyle.light(Color.TRANSPARENT, Color.TRANSPARENT)
        }
        enableEdgeToEdge(statusBarStyle = style, navigationBarStyle = style)
        onDispose { }
    }
}

private data class Tab(val title: String, val icon: ImageVector)

private val tabs = listOf(
    Tab("Send", Lucide.Send),
    Tab("Transfers", Lucide.ArrowUpDown),
    Tab("History", Lucide.History),
    Tab("Settings", Lucide.Settings),
)

@Composable
private fun AppShell(requestedTab: Int, onTabShown: () -> Unit) {
    val core = LocalContext.current.core
    var tab by rememberSaveable { mutableIntStateOf(MainActivity.TAB_SEND) }
    val snackbar = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()
    val transfers by core.transfers.list.collectAsStateWithLifecycle()
    val starting by core.transfers.starting.collectAsStateWithLifecycle()
    val active = transfers.count { it.state.active } + starting
    val c = Rocket.colors
    val say: (String) -> Unit = { m -> scope.launch { snackbar.showSnackbar(m, withDismissAction = true) } }

    LaunchedEffect(requestedTab) {
        if (requestedTab in tabs.indices) {
            tab = requestedTab
            onTabShown()
        }
    }
    LaunchedEffect(Unit) {
        core.transfers.messages.collect { say(it) }
    }

    Scaffold(
        containerColor = c.bg,
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopBar(tabs[tab].title)
        },
        bottomBar = {
            NavigationBar(containerColor = c.bgElev) {
                tabs.forEachIndexed { i, t ->
                    val badge = i == MainActivity.TAB_TRANSFERS && active > 0
                    NavigationBarItem(
                        selected = tab == i,
                        onClick = { tab = i },
                        icon = {
                            if (badge) {
                                BadgedBox(badge = {
                                    Badge(containerColor = c.accent, contentColor = c.accentFg) { Text("$active") }
                                }) { Icon(t.icon, contentDescription = null) }
                            } else {
                                Icon(t.icon, contentDescription = null)
                            }
                        },
                        label = { Text(t.title) },
                        modifier = if (badge) {
                            Modifier.semantics { contentDescription = "${t.title}, $active in progress" }
                        } else {
                            Modifier
                        },
                        colors = NavigationBarItemDefaults.colors(
                            selectedIconColor = c.accent,
                            selectedTextColor = c.fg,
                            indicatorColor = c.bgElev2,
                            unselectedIconColor = c.fgMuted,
                            unselectedTextColor = c.fgMuted,
                        ),
                    )
                }
            }
        },
    ) { padding ->
        val m = Modifier.fillMaxSize().padding(padding).consumeWindowInsets(padding)
        when (tab) {
            MainActivity.TAB_SEND -> SendScreen(onMessage = say, modifier = m)
            MainActivity.TAB_TRANSFERS -> TransfersScreen(modifier = m)
            MainActivity.TAB_HISTORY -> HistoryScreen(onMessage = say, modifier = m)
            else -> SettingsScreen(onMessage = say, modifier = m)
        }
    }
}
