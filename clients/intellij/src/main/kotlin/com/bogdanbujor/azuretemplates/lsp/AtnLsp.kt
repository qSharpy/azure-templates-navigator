package com.bogdanbujor.azuretemplates.lsp

// LSP4IJ wiring for the shared `atn` language server. All language features
// (hover, completion, diagnostics, quick fixes, go-to-definition) come from
// the server through LSP4IJ; this file only starts the binary and exposes the
// custom `atn/*` requests used by the tool windows.

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.settings.AtnSettings
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.intellij.execution.configurations.GeneralCommandLine
import com.intellij.ide.plugins.PluginManagerCore
import com.intellij.openapi.extensions.PluginId
import com.intellij.openapi.project.Project
import com.intellij.openapi.util.SystemInfo
import com.intellij.openapi.vfs.VirtualFile
import com.intellij.util.system.CpuArch
import com.redhat.devtools.lsp4ij.LanguageServerFactory
import com.redhat.devtools.lsp4ij.client.LanguageClientImpl
import com.redhat.devtools.lsp4ij.server.OSProcessStreamConnectionProvider
import com.redhat.devtools.lsp4ij.server.StreamConnectionProvider
import org.eclipse.lsp4j.jsonrpc.services.JsonNotification
import org.eclipse.lsp4j.jsonrpc.services.JsonRequest
import org.eclipse.lsp4j.services.LanguageServer
import java.io.File
import java.util.concurrent.CompletableFuture

const val SERVER_ID = "azureTemplatesNavigator"
const val PLUGIN_ID = "com.bogdanbujor.azure-templates-navigator"
const val COMMAND_PREFIX = "AzureTemplatesNavigator."

/** Custom requests implemented by the server (see server/internal/lsp). */
interface AtnLanguageServer : LanguageServer {
    @JsonRequest("atn/dependencyTree")
    fun dependencyTree(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/graph")
    fun graph(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/workspaceDiagnostics")
    fun workspaceDiagnostics(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/templates")
    fun templates(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/parameters")
    fun parameters(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/repositories")
    fun repositories(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/references")
    fun references(params: JsonObject): CompletableFuture<JsonElement?>

    @JsonRequest("atn/info")
    fun info(params: JsonObject): CompletableFuture<JsonElement?>
}

class AtnServerFactory : LanguageServerFactory {
    override fun createConnectionProvider(project: Project): StreamConnectionProvider = AtnConnectionProvider()
    override fun createLanguageClient(project: Project): LanguageClientImpl = AtnLanguageClient(project)
    override fun getServerInterface(): Class<out LanguageServer> = AtnLanguageServer::class.java
}

class AtnConnectionProvider : OSProcessStreamConnectionProvider() {
    init {
        val binary = ServerBinary.locate()
        commandLine = GeneralCommandLine(binary.absolutePath, "lsp").withParentEnvironmentType(GeneralCommandLine.ParentEnvironmentType.CONSOLE)
    }

    override fun getInitializationOptions(rootUri: VirtualFile?): Any = JsonObject().apply {
        add("settings", AtnSettings.getInstance().toServerJson())
        add("client", JsonObject().apply {
            // IntelliJ renders hovers in its documentation popup: no command
            // links, no emoji, no inline HTML.
            addProperty("commandLinks", false)
            addProperty("plainMarkdown", true)
            // Code lens commands are dispatched by LSP4IJ to the IntelliJ
            // action with the same id (see ShowUsagesAction).
            addProperty("commandPrefix", COMMAND_PREFIX)
        })
    }
}

class AtnLanguageClient(project: Project) : LanguageClientImpl(project) {
    /** Sent by the server whenever the index or diagnostics changed. */
    @JsonNotification("atn/didChange")
    fun didChange(@Suppress("UNUSED_PARAMETER") params: JsonElement?) {
        if (!project.isDisposed) AtnService.getInstance(project).fireChanged()
    }

    override fun createSettings(): Any = AtnSettings.getInstance().toServerJson()

    override fun logMessage(message: org.eclipse.lsp4j.MessageParams) {
        LOG.info("[atn] " + message.message)
        super.logMessage(message)
    }

    private companion object {
        val LOG = com.intellij.openapi.diagnostic.logger<AtnLanguageClient>()
    }
}

/** Starts the server when a project opens so repositories are fetched and
 *  project-wide diagnostics are available before any YAML file is opened
 *  (mirrors the VS Code `workspaceContains` activation). */
class AtnStartupActivity : com.intellij.openapi.startup.ProjectActivity {
    override suspend fun execute(project: Project) {
        val base = project.basePath ?: return
        val hasYaml = File(base).walkTopDown().maxDepth(3)
            .onEnter { it.name !in setOf(".git", "node_modules", ".idea", "build", ".gradle") }
            .any { it.isFile && (it.name.endsWith(".yml") || it.name.endsWith(".yaml")) }
        if (hasYaml) com.redhat.devtools.lsp4ij.LanguageServerManager.getInstance(project).start(SERVER_ID)
    }
}

/** Finds the bundled `atn` binary: <plugin>/bin/<platform>-<arch>/atn[.exe]. */
object ServerBinary {
    val target: String
        get() {
            val os = when {
                SystemInfo.isMac -> "darwin"
                SystemInfo.isWindows -> "win32"
                else -> "linux"
            }
            val arch = if (CpuArch.isArm64()) "arm64" else "x64"
            return "$os-$arch"
        }

    fun locate(): File {
        AtnSettings.getInstance().state.serverPath.takeIf { it.isNotBlank() }?.let { return File(it) }
        val exe = if (SystemInfo.isWindows) "atn.exe" else "atn"
        val pluginDir = PluginManagerCore.getPlugin(PluginId.getId(PLUGIN_ID))?.pluginPath?.toFile()
            ?: throw IllegalStateException("Azure Templates Navigator plugin directory not found")
        val file = File(pluginDir, "bin/$target/$exe")
        if (!file.exists()) throw IllegalStateException("Language server binary not found for $target: $file")
        if (!file.canExecute()) file.setExecutable(true)
        return file
    }
}
