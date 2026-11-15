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
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import io.github.donaldsteele.rocket.core
import io.github.donaldsteele.rocket.core.ReceiverState
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

    override fun onStart() {
        super.onStart()
        onScreen = true
        // Receiving was on when the system stopped the app: turn it back on.
        val c = core
        if (c.receiveWanted && c.receiver.state.value == ReceiverState.Off) {
            runCatching { c.startReceiving(this) }
        }
    }

    override fun onStop() {
        onScreen = false
        super.onStop()
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
        const val TAB_RECEIVE = 1
        const val TAB_TRANSFERS = 2
        const val TAB_HISTORY = 3
        const val TAB_SETTINGS = 4

        /**
         * True while this activity is started, so it shows offers itself
         * (sheet or Receive tab). Main thread writes, Go threads read.
         */
        @Volatile
        var onScreen = false
            private set
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

/** A bottom-bar label: one line, grown at most to [NAV_LABEL_MAX_SCALE]. */
@Composable
private fun NavLabel(text: String) {
    val base = MaterialTheme.typography.labelMedium.fontSize
    val factor = navLabelFactor(LocalDensity.current.fontScale)
    Text(
        text,
        fontSize = if (base.isSp) base * factor else base,
        maxLines = 1,
        softWrap = false,
        overflow = TextOverflow.Ellipsis,
    )
}

private data class Tab(val title: String, val icon: ImageVector)

private val tabs = listOf(
    Tab("Send", Lucide.Send),
    Tab("Receive", Lucide.Download),
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
    val offers by core.receiver.offers.collectAsStateWithLifecycle()
    // Offers whose sheet was swiped away; they stay in the Receive tab.
    val hidden = remember { mutableStateListOf<String>() }
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
                    val count = when (i) {
                        MainActivity.TAB_TRANSFERS -> active
                        MainActivity.TAB_RECEIVE -> offers.size
                        else -> 0
                    }
                    val badge = count > 0
                    val badgeWhat = if (i == MainActivity.TAB_RECEIVE) "waiting" else "in progress"
                    NavigationBarItem(
                        selected = tab == i,
                        onClick = { tab = i },
                        icon = {
                            if (badge) {
                                BadgedBox(badge = {
                                    Badge(containerColor = c.accent, contentColor = c.accentFg) { Text("$count") }
                                }) { Icon(t.icon, contentDescription = null) }
                            } else {
                                Icon(t.icon, contentDescription = null)
                            }
                        },
                        label = { NavLabel(t.title) },
                        modifier = if (badge) {
                            Modifier.semantics { contentDescription = "${t.title}, $count $badgeWhat" }
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
            MainActivity.TAB_RECEIVE -> ReceiveScreen(onMessage = say, modifier = m)
            MainActivity.TAB_TRANSFERS -> TransfersScreen(onMessage = say, modifier = m)
            MainActivity.TAB_HISTORY -> HistoryScreen(onMessage = say, modifier = m)
            else -> SettingsScreen(onMessage = say, modifier = m)
        }
    }

    // The Receive tab lists offers itself; elsewhere each one not
    // swiped away comes up as a sheet, oldest first.
    val sheetOffer = offers.firstOrNull { it.id !in hidden }
    if (tab != MainActivity.TAB_RECEIVE && sheetOffer != null) {
        OfferSheet(
            sheetOffer,
            onAnswer = { accept, trust -> answerOffer(core.receiver::answer, sheetOffer, accept, trust, say) },
            onDismiss = { hidden += sheetOffer.id },
        )
    }
}
