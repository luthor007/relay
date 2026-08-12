plugins {
    alias(libs.plugins.android.library)
    alias(libs.plugins.kotlin.android)
}

// ---------------------------------------------------------------- vendor SDK
//
// The vendor AAR is proprietary Shenzhen QC.wireless material and is not copied
// into this module automatically — see apps/android/README.md. Everything here
// is arranged so the module builds *identically whether or not it is present*:
//
//   absent  → src/vendor/ is excluded, only the mock transport exists
//   present → src/vendor/ compiles against the extracted classes.jar
//
// Two things force this shape. `compileOnly(files("x.aar"))` does not work at
// all — AGP resolves file dependencies as JARs, so an AAR silently contributes
// no classes and every com.glasses.* reference fails to resolve. And a source
// file that references classes which may not exist cannot live in `main`.
val vendorAar = layout.projectDirectory.file("../libs/LIB_GLASSES_SDK-release-20260709_8.aar").asFile
val hasVendorSdk = vendorAar.exists()

// Registered only when the AAR is present, rather than registered-and-skipped:
// an `onlyIf { }` lambda closes over this script, and the configuration cache
// cannot serialise script references.
val extractVendorSdk = if (hasVendorSdk) {
    tasks.register<Copy>("extractVendorSdk") {
        from(zipTree(vendorAar)) { include("classes.jar") }
        into(layout.buildDirectory.dir("vendor"))
        rename("classes.jar", "vendor-sdk.jar")
    }
} else {
    null
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

    sourceSets {
        getByName("main") {
            // Only compiled when the AAR is on disk. The factory in `main` finds
            // this class reflectively, so `main` never depends on it existing.
            if (hasVendorSdk) java.srcDir("src/vendor/java")
        }
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

    // Recorded so the factory can tell "mock because debug" from "mock because
    // the SDK was missing at build time" — the second is a broken release, and
    // it should fail loudly rather than ship a build that cannot see glasses.
    buildTypes.configureEach {
        buildConfigField("boolean", "HAS_VENDOR_SDK", hasVendorSdk.toString())
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
    implementation(libs.androidx.core.ktx)
    implementation(libs.kotlinx.coroutines.android)

    // Device-only (arm64), so it is compiled against but never packaged: the
    // app module links the real AAR. See apps/android/README.md.
    if (extractVendorSdk != null) {
        compileOnly(files(layout.buildDirectory.file("vendor/vendor-sdk.jar")) {
            builtBy(extractVendorSdk)
        })
    }

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)

    // A working org.json on the unit-test classpath.
    //
    // android.jar's org.json is a stub whose every method throws, and
    // `unitTests.isReturnDefaultValues = true` above converts that throw into a
    // null return — so `JSONObject.put(k, 1)` hands back null instead of `this`,
    // and the next call in the chain dies on a NullPointerException inside
    // Envelope.kt. It cost 51 tests across Envelope, Outbox and RelaydLink, all
    // of them reporting an NPE in code that is correct.
    //
    // This real implementation comes first on the classpath and shadows the
    // stub. It is test-only: the device already has org.json in the framework.
    testImplementation(libs.json)
}
