package io.github.donaldsteele.rocket

import android.app.Application
import android.content.Context
import io.github.donaldsteele.rocket.core.Core

class RocketApp : Application() {
    /** Made on first use: loading the Go core is not free. */
    val core: Core by lazy { Core(this) }
}

/** The app-wide [Core]. */
val Context.core: Core get() = (applicationContext as RocketApp).core
