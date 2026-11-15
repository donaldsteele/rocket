package io.github.donaldsteele.rocket.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Shapes
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.ExperimentalTextApi
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontVariation
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import io.github.donaldsteele.rocket.R
import io.github.donaldsteele.rocket.core.ThemeMode

/**
 * rocket's warm design tokens, as in web/styles.css (shared with desktop
 * rocket and netdiag). Contrast there is checked to WCAG AA: text pairs at
 * least 4.5:1, UI and icon pairs at least 3:1.
 */
@Immutable
data class RocketColors(
    val bg: Color,
    val bgElev: Color,
    val bgElev2: Color,
    val line: Color,
    val lineStrong: Color,
    val fg: Color,
    val fgMuted: Color,
    val accent: Color,
    val accentFg: Color,
    val link: Color,
    val ok: Color,
    val warn: Color,
    val bad: Color,
    val info: Color,
    val focus: Color,
    val warnBg: Color,
    val badBg: Color,
    val okBg: Color,
    val infoBg: Color,
    val isDark: Boolean,
)

val DarkTokens = RocketColors(
    bg = Color(0xFF1C1917), bgElev = Color(0xFF292524), bgElev2 = Color(0xFF231F1D),
    line = Color(0xFF44403C), lineStrong = Color(0xFF78716C),
    fg = Color(0xFFF5F0E8), fgMuted = Color(0xFFB8AEA1),
    accent = Color(0xFFFB923C), accentFg = Color(0xFF1C1917), link = Color(0xFFFDBA74),
    ok = Color(0xFF34D399), warn = Color(0xFFFACC15), bad = Color(0xFFF87171), info = Color(0xFFA78BFA),
    focus = Color(0xFFFDE68A),
    warnBg = Color(0xFF3A2F0D), badBg = Color(0xFF3B1A1A), okBg = Color(0xFF12291F), infoBg = Color(0xFF261D3D),
    isDark = true,
)

val LightTokens = RocketColors(
    bg = Color(0xFFFAF7F2), bgElev = Color(0xFFF3EEE6), bgElev2 = Color(0xFFEBE4D9),
    line = Color(0xFFD6CFC4), lineStrong = Color(0xFF8A8178),
    fg = Color(0xFF292524), fgMuted = Color(0xFF57534E),
    accent = Color(0xFFC2410C), accentFg = Color(0xFFFFFFFF), link = Color(0xFF9A3412),
    ok = Color(0xFF166534), warn = Color(0xFF854D0E), bad = Color(0xFFB91C1C), info = Color(0xFF6D28D9),
    focus = Color(0xFF1D4ED8),
    warnBg = Color(0xFFFEF3C7), badBg = Color(0xFFFEE2E2), okBg = Color(0xFFDCFCE7), infoBg = Color(0xFFEDE9FE),
    isDark = false,
)

private fun RocketColors.scheme(): ColorScheme {
    val base = if (isDark) darkColorScheme() else lightColorScheme()
    return base.copy(
        primary = accent, onPrimary = accentFg,
        primaryContainer = bgElev2, onPrimaryContainer = fg,
        inversePrimary = link,
        secondary = link, onSecondary = bg,
        secondaryContainer = bgElev2, onSecondaryContainer = fg,
        tertiary = info, onTertiary = bg,
        tertiaryContainer = infoBg, onTertiaryContainer = fg,
        background = bg, onBackground = fg,
        surface = bg, onSurface = fg,
        surfaceVariant = bgElev2, onSurfaceVariant = fgMuted,
        surfaceTint = accent,
        inverseSurface = fg, inverseOnSurface = bg,
        error = bad, onError = bg,
        errorContainer = badBg, onErrorContainer = fg,
        outline = lineStrong, outlineVariant = line,
        scrim = Color.Black,
        surfaceBright = bgElev, surfaceDim = bg,
        surfaceContainerLowest = bg, surfaceContainerLow = bgElev2,
        surfaceContainer = bgElev, surfaceContainerHigh = bgElev,
        surfaceContainerHighest = bgElev2,
    )
}

/** Inter (variable, SIL OFL), the UI face. Latin subset from web/vendor/fonts. */
@OptIn(ExperimentalTextApi::class)
val Inter = FontFamily(
    listOf(400, 500, 600, 700).map { w ->
        Font(
            R.font.inter_variable,
            weight = FontWeight(w),
            variationSettings = FontVariation.Settings(FontVariation.weight(w)),
        )
    },
)

/** JetBrains Mono (SIL OFL), for fingerprints, addresses and file names. */
val Mono = FontFamily(
    Font(R.font.jetbrains_mono_regular, FontWeight.Normal),
    Font(R.font.jetbrains_mono_bold, FontWeight.Bold),
)

private fun typography(): Typography {
    val t = Typography()
    fun TextStyle.inter() = copy(fontFamily = Inter)
    return Typography(
        displayLarge = t.displayLarge.inter(), displayMedium = t.displayMedium.inter(), displaySmall = t.displaySmall.inter(),
        headlineLarge = t.headlineLarge.inter(), headlineMedium = t.headlineMedium.inter(),
        headlineSmall = t.headlineSmall.inter().copy(fontWeight = FontWeight.Bold),
        titleLarge = t.titleLarge.inter().copy(fontSize = 20.sp, fontWeight = FontWeight.Bold),
        titleMedium = t.titleMedium.inter().copy(fontWeight = FontWeight.SemiBold),
        titleSmall = t.titleSmall.inter().copy(fontWeight = FontWeight.SemiBold),
        bodyLarge = t.bodyLarge.inter().copy(fontSize = 16.sp, lineHeight = 23.sp),
        bodyMedium = t.bodyMedium.inter().copy(fontSize = 15.sp, lineHeight = 22.sp),
        bodySmall = t.bodySmall.inter().copy(fontSize = 13.sp, lineHeight = 18.sp),
        labelLarge = t.labelLarge.inter().copy(fontSize = 15.sp, fontWeight = FontWeight.SemiBold),
        labelMedium = t.labelMedium.inter().copy(fontWeight = FontWeight.SemiBold),
        labelSmall = t.labelSmall.inter(),
    )
}

private val RocketTypography by lazy { typography() }

private val RocketShapes = Shapes(
    extraSmall = RoundedCornerShape(4.dp),
    small = RoundedCornerShape(4.dp),
    medium = RoundedCornerShape(6.dp),
    large = RoundedCornerShape(8.dp),
    extraLarge = RoundedCornerShape(12.dp),
)

private val LocalRocketColors = staticCompositionLocalOf { DarkTokens }

/** The rocket tokens of the current theme. */
object Rocket {
    val colors: RocketColors
        @Composable @ReadOnlyComposable
        get() = LocalRocketColors.current
}

/** Whether mode resolves to the dark theme here. */
@Composable
fun ThemeMode.isDark(): Boolean = when (this) {
    ThemeMode.Dark -> true
    ThemeMode.Light -> false
    ThemeMode.System -> isSystemInDarkTheme()
}

@Composable
fun RocketTheme(mode: ThemeMode, content: @Composable () -> Unit) {
    val tokens = if (mode.isDark()) DarkTokens else LightTokens
    CompositionLocalProvider(LocalRocketColors provides tokens) {
        MaterialTheme(
            colorScheme = tokens.scheme(),
            typography = RocketTypography,
            shapes = RocketShapes,
            content = content,
        )
    }
}
