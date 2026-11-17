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
// Shared tips (../../shared/tips/tips.json) -> classpath resources /tips/AzureTemplates_<id>.html,
// in the format of the IDE's own Tip of the Day files. plugin.xml lists them (TipsParityTest checks it).
fun tipMarkup(s: String): String = s
    .replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    .replace(Regex("""\{shortcut:([A-Za-z0-9_.]+)}"""), """<span class="shortcut">&shortcut:$1;</span>""")
    .replace(Regex("""`([^`]+)`"""), "<code>$1</code>")
    .replace(Regex("""\*\*([^*]+)\*\*"""), "<b>$1</b>")
    .replace(Regex("""\*([^*]+)\*"""), """<span class="control">$1</span>""")
    .replace(Regex("""\[([^\]]+)]\((https://[^)]+)\)"""), """<a href="$2">$1</a>""")

val generateTips by tasks.registering {
    val src = repoRoot.file("shared/tips/tips.json")
    // A tip's screenshot, when there is one: ../../docs/images/intellij/<id>.png (2x). The IDE picks
    // images/<name>@2x.png or @2x_dark.png for <img src="images/<name>.png">; ours is the same in both.
    val images = repoRoot.dir("docs/images/intellij")
    val out = layout.buildDirectory.dir("generated/shared/tips")
    inputs.file(src)
    inputs.dir(images).optional()
    outputs.dir(out)
    doLast {
        @Suppress("UNCHECKED_CAST")
        val tips = (groovy.json.JsonSlurper().parse(src.asFile) as Map<String, Any>)["tips"] as List<Map<String, String>>
        val dir = out.get().asFile
        dir.deleteRecursively()
        dir.mkdirs()
        File(dir, "images").mkdirs()
        for (t in tips) {
            val shot = images.file("${t["id"]}.png").asFile
            val example = if (shot.exists()) {
                for (variant in listOf("@2x", "@2x_dark")) shot.copyTo(File(dir, "images/AzureTemplates_${t["id"]}$variant.png"), overwrite = true)
                """<p class="image"><img src="images/AzureTemplates_${t["id"]}.png" alt="${tipMarkup(t["title"]!!)}"></p>"""
            } else {
                t["example"]?.let { "<pre>" + it.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;") + "</pre>" } ?: ""
            }
            File(dir, "AzureTemplates_${t["id"]}.html").writeText(
                """<html><head><meta charset="UTF-8"><link rel="stylesheet" type="text/css" href="css/tips.css"></head><body>""" +
                    "<p><b>${tipMarkup(t["title"]!!)}</b></p><p>${tipMarkup(t["intellij"]!!)}</p>$example</body></html>\n",
            )
        }
    }
}
tasks.processResources { dependsOn(copySharedWebview, generateTips) }

// Server binaries -> <plugin>/bin/<platform>-<arch>/atn[.exe], in every IDE sandbox (runIde, runIdeForScreenshots)
tasks.withType<org.jetbrains.intellij.platform.gradle.tasks.PrepareSandboxTask>().matching { !it.name.contains("Test") }.configureEach {
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

// Documentation screenshots (docs/images/intellij): a separate sandbox with JetBrains' UI-test robot
// server on 127.0.0.1:8082, driven by scripts/screenshots/intellij.js. It runs IDE actions by id, so it
// needs no keyboard or accessibility access.
val runIdeForScreenshots by intellijPlatformTesting.runIde.registering {
    task {
        jvmArgs(
            "-Drobot-server.port=8082",
            "-Dgradle.compatibility.update.interval=0",
            "-Dide.mac.message.dialogs.as.sheets=false",
            "-Djb.privacy.policy.text=<!--999.999-->",
            "-Djb.consents.confirmation.enabled=false",
            "-Didea.trust.all.projects=true",
            "-Dide.show.tips.on.startup.default.value=false",
        )
    }
    plugins { robotServerPlugin() }
}

tasks.wrapper { gradleVersion = "8.11.1" }
