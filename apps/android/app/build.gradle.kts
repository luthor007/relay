import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
}

// Release signing.
//
// keystore.properties is machine-local and gitignored, and the keystore it
// points at deliberately lives outside this repository — a signing key inside
// the tree is one `git add -A` away from being public forever, and this project
// has a public mirror.
//
// A checkout without that file still builds. It produces an unsigned release,
// which is the honest outcome: someone who cannot sign should still be able to
// compile, and a build that fails with "keystore.properties not found" teaches
// people to paste keys into the repo to make the error go away.
val keystorePropsFile = rootProject.file("keystore.properties")
val keystoreProps = Properties().apply {
    if (keystorePropsFile.exists()) {
        keystorePropsFile.inputStream().use { load(it) }
    }
}
val canSign = keystorePropsFile.exists() &&
    keystoreProps.getProperty("storeFile")?.let { file(it).exists() } == true

android {
    namespace = "glass.relay.app"
    compileSdk = 35

    defaultConfig {
        applicationId = "glass.relay"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    buildFeatures {
        compose = true
    }

    signingConfigs {
        if (canSign) {
            create("release") {
                storeFile = file(keystoreProps.getProperty("storeFile"))
                storePassword = keystoreProps.getProperty("storePassword")
                keyAlias = keystoreProps.getProperty("keyAlias")
                keyPassword = keystoreProps.getProperty("keyPassword")

                // v1 is JAR signing, and it only matters below API 24. minSdk
                // here is 26, so every device this app installs on verifies
                // through v2 or v3, and AGP drops v1 accordingly — apksigner
                // reports `v1: false` on the output and that is correct, not a
                // gap. Stated because "one of the three says false" is exactly
                // the kind of thing that gets a working signing config
                // "fixed" later.
                enableV2Signing = true
                enableV3Signing = true
            }
        }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
        }
        release {
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Null when there is no keystore on this machine, which leaves the
            // release unsigned rather than failing the build.
            signingConfig = signingConfigs.findByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    implementation(project(":relay-bridge"))

    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.activity.compose)
    implementation(libs.kotlinx.coroutines.android)

    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.graphics)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.material.icons)
    debugImplementation(libs.androidx.compose.ui.tooling)

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
}
