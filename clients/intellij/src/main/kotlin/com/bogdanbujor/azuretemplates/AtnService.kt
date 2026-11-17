package com.bogdanbujor.azuretemplates

import com.bogdanbujor.azuretemplates.lsp.AtnLanguageServer
import com.bogdanbujor.azuretemplates.lsp.SERVER_ID
import com.bogdanbujor.azuretemplates.settings.AtnSettings
import com.google.gson.Gson
import com.google.gson.JsonArray
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.intellij.openapi.Disposable
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.service
import com.intellij.openapi.diagnostic.logger
import com.intellij.openapi.fileEditor.OpenFileDescriptor
import com.intellij.openapi.project.Project
import com.intellij.openapi.vfs.LocalFileSystem
import com.intellij.util.Alarm
import com.intellij.util.messages.Topic
import com.redhat.devtools.lsp4ij.LanguageServerManager
import com.redhat.devtools.lsp4ij.ServerStatus
import org.eclipse.lsp4j.DidChangeConfigurationParams
import org.eclipse.lsp4j.ExecuteCommandParams
import java.util.concurrent.CompletableFuture

/** Project-level access to the `atn` server for the tool windows and actions. */
@Service(Service.Level.PROJECT)
class AtnService(private val project: Project) : Disposable {

    fun interface Listener {
        fun changed()
    }

    companion object {
        val TOPIC: Topic<Listener> = Topic.create("AzureTemplatesNavigatorChanged", Listener::class.java)
        val gson = Gson()
        private val LOG = logger<AtnService>()
        fun getInstance(project: Project): AtnService = project.service()
    }

    private val alarm = Alarm(Alarm.ThreadToUse.POOLED_THREAD, this)

    /** Coalesces change notifications and broadcasts them to the views. */
    fun fireChanged() {
        alarm.cancelAllRequests()
        alarm.addRequest({
            if (!project.isDisposed) project.messageBus.syncPublisher(TOPIC).changed()
        }, 250)
    }

    private fun server(): CompletableFuture<AtnLanguageServer?> {
        val manager = LanguageServerManager.getInstance(project)
        if (manager.getServerStatus(SERVER_ID) == ServerStatus.none || manager.getServerStatus(SERVER_ID) == ServerStatus.stopped) {
            manager.start(SERVER_ID)
        }
        return manager.getLanguageServer(SERVER_ID)
            .thenCompose { item -> item?.initializedServer ?: CompletableFuture.completedFuture(null) }
            .thenApply { it as? AtnLanguageServer }
    }

    private fun call(block: (AtnLanguageServer) -> CompletableFuture<JsonElement?>): CompletableFuture<JsonElement?> =
        server().thenCompose { s -> if (s == null) CompletableFuture.completedFuture(null) else block(s) }
            .exceptionally { e -> LOG.info("atn request failed", e); null }

    fun dependencyTree(path: String) = call { it.dependencyTree(JsonObject().apply { addProperty("uri", fileUri(path)) }) }
    /**
     * The template graph: around [path] (callers/callees up to [depth], see [direction]) or the whole project.
     * Filters: [problemsOnly]; [kinds] (pipeline|template|external|broken) and [repos] (facet keys,
     * "" = this project) are "all" when empty.
     */
    fun graph(
        path: String?, depth: Int, direction: String = "both",
        problemsOnly: Boolean = false, kinds: Collection<String> = emptyList(), repos: Collection<String> = emptyList(),
    ) = call {
        it.graph(JsonObject().apply {
            if (path != null) {
                addProperty("uri", fileUri(path))
                addProperty("depth", depth)
                addProperty("direction", direction)
            }
            addProperty("problemsOnly", problemsOnly)
            add("kinds", JsonArray().apply { kinds.forEach { k -> add(k) } })
            add("repos", JsonArray().apply { repos.forEach { r -> add(r) } })
        })
    }
    fun stats() = call { it.stats(JsonObject()) }
    /** Every file's problems; with [onlyNew] (Only My Changes) an object with only the ones your changes added. */
    fun workspaceDiagnostics(onlyNew: Boolean = false) =
        call { it.workspaceDiagnostics(JsonObject().apply { if (onlyNew) addProperty("onlyNew", true) }) }
    fun templates() = call { it.templates(JsonObject()) }
    /** Parameters of the template referenced on [line] of [path] (or of [path] itself). */
    fun parameters(path: String, line: Int) = call {
        it.parameters(JsonObject().apply { addProperty("uri", fileUri(path)); addProperty("line", line) })
    }
    fun repositories() = call { it.repositories(JsonObject()) }
    /** Usages of whatever is at [line]:[character] (UTF-16) of [path]: a parameter, a template reference, or the file. */
    fun references(path: String, line: Int, character: Int) = call {
        it.references(JsonObject().apply {
            add("textDocument", JsonObject().apply { addProperty("uri", fileUri(path)) })
            add("position", JsonObject().apply { addProperty("line", line); addProperty("character", character) })
        })
    }
    fun info() = call { it.info(JsonObject()) }

    /** Verdict of a root pipeline (null for a template): everything that would make it fail. */
    fun pipelineCheck(path: String) = call { it.pipelineCheck(JsonObject().apply { addProperty("uri", fileUri(path)) }) }

    /** Verdicts of every root pipeline, failing first (null when diagnostics are off). */
    fun pipelineChecks() = call { it.pipelineCheck(JsonObject()) }

    /** What the project changed since the commit the work started from (see atn/changes). */
    fun changes() = call { it.changes(JsonObject()) }

    /** What [changes] can compare with: automatic, uncommitted only, recent branches. */
    fun changeBases() = call { it.changeBases(JsonObject()) }

    /**
     * Compare with [ref] from now on ("" = automatic, "HEAD" = uncommitted changes only).
     * Completes exceptionally with the server's reason when [ref] can't be used.
     */
    fun setChangesBase(ref: String): CompletableFuture<JsonElement?> =
        server().thenCompose { s ->
            s?.setChangesBase(JsonObject().apply { addProperty("ref", ref) }) ?: CompletableFuture.completedFuture(null)
        }

    /** What a check in Azure DevOps would do (reads only): a plan, or {error}. */
    fun azurePlan() = call { it.azurePlan(JsonObject()) }

    /** Starts the check of the plan the user confirmed: {started} or {error}. */
    fun azureValidate(planId: String) = call { it.azureValidate(JsonObject().apply { addProperty("planId", planId) }) }

    fun azureCancel() = call { it.azureCancel(JsonObject()) }

    /** A pipeline's expanded YAML at the base and with your changes: {base, change, title}. */
    fun azureExpansion(path: String) = call { it.azureExpansion(JsonObject().apply { addProperty("uri", fileUri(path)) }) }

    /** Listener for atn/azureProgress, set while a check runs. */
    @Volatile
    var onAzureProgress: ((JsonObject) -> Unit)? = null

    fun azureProgress(params: JsonElement?) {
        params.obj()?.let { p -> onAzureProgress?.invoke(p) }
    }

    fun execute(command: String, vararg args: Any): CompletableFuture<Any?> =
        server().thenCompose { s ->
            s?.workspaceService?.executeCommand(ExecuteCommandParams(command, args.toList()))
                ?: CompletableFuture.completedFuture(null)
        }.exceptionally { null }

    fun fetchRepository(alias: String, file: String?) = execute("atn.fetchRepository", JsonObject().apply {
        addProperty("alias", alias)
        if (file != null) addProperty("file", file)
    }).thenRun { fireChanged() }

    fun pushSettings(): CompletableFuture<Void> =
        server().thenAccept { s ->
            s?.workspaceService?.didChangeConfiguration(DidChangeConfigurationParams(AtnSettings.getInstance().toServerJson()))
        }

    fun restart() {
        val manager = LanguageServerManager.getInstance(project)
        manager.stop(SERVER_ID)
        manager.start(SERVER_ID)
        fireChanged()
    }

    fun openFile(path: String?, line: Int = 0, column: Int = 0) {
        if (path.isNullOrBlank()) return
        ApplicationManager.getApplication().executeOnPooledThread {
            val vf = LocalFileSystem.getInstance().refreshAndFindFileByPath(path) ?: return@executeOnPooledThread
            ApplicationManager.getApplication().invokeLater {
                if (!project.isDisposed) OpenFileDescriptor(project, vf, maxOf(line, 0), maxOf(column, 0)).navigate(true)
            }
        }
    }

    override fun dispose() {}
}

fun fileUri(path: String): String = java.nio.file.Paths.get(path).toUri().toString()

// ── JSON helpers ──────────────────────────────────────────────────────────
fun JsonElement?.obj(): JsonObject? = if (this != null && isJsonObject) asJsonObject else null
fun JsonElement?.arr(): JsonArray = if (this != null && isJsonArray) asJsonArray else JsonArray()
fun JsonObject.str(key: String): String = get(key)?.takeIf { !it.isJsonNull }?.asString ?: ""
fun JsonObject.int(key: String): Int = get(key)?.takeIf { !it.isJsonNull }?.asInt ?: 0
fun JsonObject.list(key: String): List<JsonObject> = get(key).arr().mapNotNull { it.obj() }
