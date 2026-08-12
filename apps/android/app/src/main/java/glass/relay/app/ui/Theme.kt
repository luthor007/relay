package glass.relay.app.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.sp

val Ground = Color(0xFF0A0B0D)
val Surface = Color(0xFF131519)
val Line = Color(0xFF23262C)
val Ink = Color(0xFFEFEDE8)
val InkMid = Color(0xFFA9ADB4)
val InkDim = Color(0xFF797E86)

/** Recording amber. Used for one thing only — see colors.xml. */
val Live = Color(0xFFE9A23B)

private val RelayColors = darkColorScheme(
    primary = Ink,
    onPrimary = Ground,
    secondary = InkMid,
    background = Ground,
    onBackground = Ink,
    surface = Surface,
    onSurface = Ink,
    surfaceVariant = Surface,
    onSurfaceVariant = InkMid,
    outline = Line,
    error = Live,
)

private val RelayType = Typography(
    displaySmall = TextStyle(fontSize = 32.sp, fontWeight = FontWeight.Medium, letterSpacing = (-0.5).sp),
    headlineSmall = TextStyle(fontSize = 22.sp, fontWeight = FontWeight.Medium),
    bodyLarge = TextStyle(fontSize = 16.sp, lineHeight = 24.sp),
    bodyMedium = TextStyle(fontSize = 14.sp, lineHeight = 21.sp),
    labelLarge = TextStyle(fontSize = 14.sp, fontWeight = FontWeight.Medium, letterSpacing = 0.4.sp),
    labelSmall = TextStyle(fontSize = 11.sp, fontWeight = FontWeight.Medium, letterSpacing = 1.sp),
)

/**
 * Dark regardless of system setting — [isSystemInDarkTheme] is deliberately not
 * consulted. See themes.xml for why the recording indicator drives this.
 */
@Composable
fun RelayTheme(content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = RelayColors,
        typography = RelayType,
        content = content,
    )
}
