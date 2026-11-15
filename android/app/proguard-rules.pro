# The Go core (gomobile bind, Java package ...core.rocketcore) calls into
# these classes through JNI by name. App classes that implement its
# interfaces (Source, ProgressListener, ...) keep those method names because
# the interfaces are kept.
-keep class go.** { *; }
-keep class io.github.donaldsteele.rocket.core.rocketcore.** { *; }
