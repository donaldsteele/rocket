import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// The Go core (mobile/rocketcore), built by scripts/build-android.* with
// gomobile bind. It is generated, so it is not in git.
val rocketcoreAar: File = file("libs/rocketcore.aar")

// Release signing reads keystore.properties (storeFile, storePassword,
// keyAlias, keyPassword) from $ROCKET_KEYSTORE_PROPS, else from
// ~/.rocket-signing/keystore.properties. Without that file the release build
// is left unsigned; debug builds never need it. Values are only handed to the
// signing config, never printed.
val keystoreProps: File = providers.environmentVariable("ROCKET_KEYSTORE_PROPS").orNull
    ?.takeIf { it.isNotBlank() }
    ?.let { File(it) }
    ?: File(System.getProperty("user.home"), ".rocket-signing/keystore.properties")

android {
    namespace = "io.github.donaldsteele.rocket"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.github.donaldsteele.rocket"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "1.0.0"
        // The ABIs the Go core is built for (scripts/build-android.*). Without
        // this, a library's 32-bit x86 .so would make the APK installable
        // on x86 devices that have no libgojni.so, where it would crash.
        ndk { abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64") }
    }

    signingConfigs {
        if (keystoreProps.isFile) {
            create("release") {
                val p = Properties()
                keystoreProps.inputStream().use { p.load(it) }
                fun prop(key: String): String = p.getProperty(key)?.takeIf { it.isNotEmpty() }
                    ?: throw GradleException("$keystoreProps has no $key")
                val store = File(prop("storeFile"))
                storeFile = if (store.isAbsolute) store else File(keystoreProps.parentFile, store.path)
                storePassword = prop("storePassword")
                keyAlias = prop("keyAlias")
                keyPassword = prop("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            signingConfig = signingConfigs.findByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    packaging {
        resources.excludes += "/META-INF/{AL2.0,LGPL2.1}"
    }

    lint {
        abortOnError = true
        checkDependencies = false
        // Versions are pinned in gradle/libs.versions.toml on purpose: the
        // newest AndroidX releases need compileSdk 37, and the app targets
        // the installed platform 36.
        disable += setOf("GradleDependency", "NewerVersionAvailable", "AndroidGradlePluginVersion", "OldTargetApi")
    }
}

val checkRocketcore = tasks.register("checkRocketcore") {
    description = "Fails with a hint when the gomobile AAR has not been built."
    val aar = rocketcoreAar
    doLast {
        if (!aar.isFile) {
            throw GradleException(
                "$aar is missing: build it first with scripts/build-android.ps1 -AarOnly " +
                    "(or bash scripts/build-android.sh --aar-only) from the repository root.",
            )
        }
    }
}
tasks.named("preBuild") { dependsOn(checkRocketcore) }

dependencies {
    implementation(files(rocketcoreAar))

    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.foundation)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.ui.tooling.preview)
    debugImplementation(libs.androidx.compose.ui.tooling)

    testImplementation(libs.junit)
    // Android's org.json is a stub in local unit tests; this is the real one.
    testImplementation(libs.org.json)
}
