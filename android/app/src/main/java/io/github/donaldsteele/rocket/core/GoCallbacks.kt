package io.github.donaldsteele.rocket.core

import android.util.Log

/**
 * Runs the body of a Go callback that has no error result, and returns
 * fallback if it throws.
 *
 * gomobile's JNI proxy only fetches and clears a Java exception for methods
 * that return an error. An exception escaping any other callback (fd(),
 * ask(), onEvent(), count(), ...) would stay pending on the Go thread: the
 * call returns 0 or null to Go, and the next JNI call from that thread runs
 * with the exception still pending, which aborts the app or worse. Every
 * such callback is wrapped in this, with a fallback Go copes with (fd -1
 * means "use write", for example).
 */
internal inline fun <T> goCallback(what: String, fallback: T, body: () -> T): T =
    try {
        body()
    } catch (t: Throwable) {
        reportCallbackFailure(what, t)
        fallback
    }

/** Logs a failed callback (quietly in local unit tests, where Log is a stub). */
internal fun reportCallbackFailure(what: String, t: Throwable) {
    runCatching { Log.w("rocket", "$what failed", t) }
}
