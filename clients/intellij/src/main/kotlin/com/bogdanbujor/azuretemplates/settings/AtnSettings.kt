package com.bogdanbujor.azuretemplates.settings

import com.bogdanbujor.azuretemplates.AtnService
import com.google.gson.JsonObject
import com.intellij.openapi.components.PersistentStateComponent
import com.intellij.openapi.components.Service
import com.intellij.openapi.components.State
import com.intellij.openapi.components.Storage
import com.intellij.openapi.components.service
import com.intellij.openapi.options.BoundConfigurable
import com.intellij.openapi.project.ProjectManager
import com.intellij.openapi.ui.DialogPanel
import com.intellij.ui.dsl.builder.AlignX
import com.intellij.ui.dsl.builder.bindIntText
import com.intellij.ui.dsl.builder.bindSelected
import com.intellij.ui.dsl.builder.bindText
import com.intellij.ui.dsl.builder.panel
import com.intellij.ui.dsl.builder.rows

/**
 * Settings mirror the VS Code settings 1:1 (same names, same JSON sent to
 * the server) so behaviour is identical across IDEs.
 */
@Service(Service.Level.APP)
@State(name = "AzureTemplatesNavigatorSettings", storages = [Storage("azure-templates-navigator.xml")])
class AtnSettings : PersistentStateComponent<AtnSettings.Data> {

    class Data {
        var requiredParameterColor: String = "#c92d35"
        var diagnosticsEnabled: Boolean = true
        var unusedParameters: Boolean = true
        var graphRootPath: String = ""
        var autoFetch: Boolean = true
        var cacheDir: String = ""
        var refreshIntervalHours: Int = 12
        var preferLocalClones: Boolean = true
        /** One `key=url` per line. */
        var urls: String = ""
        /** One `key=path` per line. */
        var localPaths: String = ""
        var azureDevOpsOrganization: String = ""
        var azureDevOpsProject: String = ""
        var githubEnterpriseHost: String = ""
        var gitPath: String = "git"
        var serverPath: String = ""
    }

    private var data = Data()

    override fun getState(): Data = data
    override fun loadState(state: Data) {
        data = state
    }

    fun toServerJson(): JsonObject = JsonObject().apply {
        val d = data
        addProperty("requiredParameterColor", d.requiredParameterColor)
        add("diagnostics", JsonObject().apply {
            addProperty("enabled", d.diagnosticsEnabled)
            addProperty("unusedParameters", d.unusedParameters)
        })
        add("graph", JsonObject().apply { addProperty("rootPath", d.graphRootPath) })
        add("repositories", JsonObject().apply {
            addProperty("autoFetch", d.autoFetch)
            addProperty("cacheDir", d.cacheDir)
            addProperty("refreshIntervalHours", d.refreshIntervalHours)
            addProperty("preferLocalClones", d.preferLocalClones)
            add("urls", parseMap(d.urls))
            add("localPaths", parseMap(d.localPaths))
            addProperty("azureDevOpsOrganization", d.azureDevOpsOrganization)
            addProperty("azureDevOpsProject", d.azureDevOpsProject)
            addProperty("githubEnterpriseHost", d.githubEnterpriseHost)
            addProperty("gitPath", d.gitPath)
        })
    }

    private fun parseMap(text: String) = JsonObject().apply {
        text.lines().map { it.trim() }.filter { it.isNotEmpty() && !it.startsWith("#") }.forEach { line ->
            val i = line.indexOf('=')
            if (i > 0) addProperty(line.substring(0, i).trim(), line.substring(i + 1).trim())
        }
    }

    companion object {
        fun getInstance(): AtnSettings = service()
    }
}

class AtnSettingsConfigurable : BoundConfigurable("Azure Templates Navigator") {
    private val s get() = AtnSettings.getInstance().state
    private var serverPathBefore = ""

    override fun createPanel(): DialogPanel {
        serverPathBefore = s.serverPath
        return panel {
            group("Validation") {
                row { checkBox("Validate template call sites").bindSelected(s::diagnosticsEnabled) }
                row { checkBox("Warn about unused template parameters").bindSelected(s::unusedParameters) }
            }
            group("Template Graph") {
                row("Workspace graph root:") { textField().bindText(s::graphRootPath).comment("Sub-folder shown in the workspace graph; empty = whole project") }
            }
            group("Repositories (template: x.yml@alias)") {
                row { checkBox("Download repositories automatically").bindSelected(s::autoFetch).comment("Uses your normal git credentials (SSH keys, Git Credential Manager, gh auth)") }
                row { checkBox("Prefer clones next to the project (../<repo-name>)").bindSelected(s::preferLocalClones) }
                row("Cache folder:") { textField().bindText(s::cacheDir).align(AlignX.FILL).comment("Empty = ~/.azure-templates-navigator/repos (shared with VS Code and the atn CLI)") }
                row("Refresh after (hours):") { intTextField(0..10000).bindIntText(s::refreshIntervalHours).comment("0 = never re-fetch automatically") }
                row("Clone URL overrides:") {}
                row { textArea().rows(3).bindText(s::urls).align(AlignX.FILL).comment("One per line: <alias | org/repo | repo>=<clone URL>. Can also be committed in .azure-templates-navigator.json") }
                row("Local paths:") {}
                row { textArea().rows(2).bindText(s::localPaths).align(AlignX.FILL).comment("One per line: <alias | org/repo | repo>=<folder>") }
                row("Azure DevOps organization:") { textField().bindText(s::azureDevOpsOrganization).comment("Only needed when it can't be inferred from the project's git remote") }
                row("Azure DevOps project:") { textField().bindText(s::azureDevOpsProject) }
                row("GitHub Enterprise host:") { textField().bindText(s::githubEnterpriseHost) }
                row("git executable:") { textField().bindText(s::gitPath) }
            }
            group("Advanced") {
                row("Language server binary:") { textField().bindText(s::serverPath).align(AlignX.FILL).comment("Empty = bundled binary") }
            }
        }
    }

    override fun apply() {
        super.apply()
        val restart = s.serverPath != serverPathBefore
        serverPathBefore = s.serverPath
        for (project in ProjectManager.getInstance().openProjects) {
            val svc = AtnService.getInstance(project)
            if (restart) svc.restart() else svc.pushSettings()
        }
    }
}
