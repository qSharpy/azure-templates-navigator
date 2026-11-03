// Azure Templates Navigator — IntelliJ client.
// A thin shell around the shared `atn` language server (../../server),
// connected through LSP4IJ. The server binaries and the shared graph webview
// are copied in from ../../dist/bin and ../../shared/webview at build time.
plugins {
    id("java")
    id("org.jetbrains.kotlin.jvm") version "1.9.25"
    id("org.jetbrains.intellij.platform") version "2.2.1"
}

group = "com.bogdanbujor"
version = providers.gradleProperty("pluginVersion").get()

val repoRoot = layout.projectDirectory.dir("../..")

repositories {
    mavenCentral()
    intellijPlatform { defaultRepositories() }
}

dependencies {
    intellijPlatform {
        intellijIdeaCommunity("2024.2.4")
        bundledPlugin("org.jetbrains.plugins.yaml")
        plugin("com.redhat.devtools.lsp4ij:0.21.0")
        instrumentationTools()
    }
    testImplementation("junit:junit:4.13.2")
}

kotlin { jvmToolchain(17) }

intellijPlatform {
    pluginConfiguration {
        name = "Azure Templates Navigator"
        ideaVersion {
            sinceBuild = "242"
            untilBuild = provider { null }
        }
    }
    publishing { token = providers.environmentVariable("PUBLISH_TOKEN") }
}

// Shared webview (graph) -> classpath resources /webview/*
sourceSets.main { resources.srcDir(layout.buildDirectory.dir("generated/shared")) }
val copySharedWebview by tasks.registering(Copy::class) {
    from(repoRoot.dir("shared/webview"))
    into(layout.buildDirectory.dir("generated/shared/webview"))
}
tasks.processResources { dependsOn(copySharedWebview) }

// Server binaries -> <plugin>/bin/<platform>-<arch>/atn[.exe]
tasks.prepareSandbox {
    from(repoRoot.dir("dist/bin")) { into("${rootProject.name}/bin") }
    doFirst {
        if (!repoRoot.dir("dist/bin").asFile.exists()) {
            throw GradleException("Server binaries missing: run ../../scripts/build.sh server first")
        }
    }
}

// The sandbox IDE (2024.2) ships a Gradle plugin that downloads a Gradle/JDK
// compatibility list from JetBrains. The current list mentions Java 25, which
// 2024.2 cannot parse, so every sandbox start logged
// "PluginException: Cannot init component state (GradleJvmSupportMatrix) ...
// IllegalArgumentException: 25". It has nothing to do with this plugin and the
// sandbox doesn't need the list: an update interval of 0 turns the download off
// (registry keys can be overridden with -D system properties).
tasks.runIde { jvmArgs("-Dgradle.compatibility.update.interval=0") }

tasks.wrapper { gradleVersion = "8.11.1" }
