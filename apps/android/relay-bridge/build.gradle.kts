plugins {
    id("com.android.library")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "glass.relay.bridge"
    compileSdk = 35

    defaultConfig {
        minSdk = 26          // Notification channels; below this the FGS story falls apart
        targetSdk = 35       // Typed foreground services are mandatory from 34

        consumerProguardFiles("consumer-rules.pro")
    }

    buildFeatures {
        buildConfig = true
    }

    buildTypes {
        debug {
            // The vendor AAR needs real hardware, so debug builds default to the
            // mock. This is what lets the whole product surface be developed and
            // instrumented without a pair of glasses on the desk.
            buildConfigField("boolean", "USE_MOCK_GLASSES", "true")
        }
        release {
            buildConfigField("boolean", "USE_MOCK_GLASSES", "false")
            isMinifyEnabled = false
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
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")

    // Vendor SDK. Device-only (arm64); see apps/android/README.md for why debug
    // builds do not link against it.
    compileOnly(files("../libs/LIB_GLASSES_SDK-release-20260709_8.aar"))

    testImplementation("junit:junit:4.13.2")
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:1.8.1")
}
