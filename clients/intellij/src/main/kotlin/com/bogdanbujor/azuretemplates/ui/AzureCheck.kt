package com.bogdanbujor.azuretemplates.ui

// Check in Azure DevOps (server: engine/azure.go). The server makes a plan
// (reads only), the user confirms it here, and the server runs exactly that
// plan: atn/azureValidate refuses another one. Progress arrives as
// atn/azureProgress notifications; the Pipelines tab shows the results.

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.int
import com.bogdanbujor.azuretemplates.list
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.settings.AtnSettings
import com.bogdanbujor.azuretemplates.str
import com.google.gson.JsonObject
import com.intellij.diff.DiffContentFactory
import com.intellij.diff.DiffManager
import com.intellij.diff.requests.SimpleDiffRequest
import com.intellij.ide.util.PropertiesComponent
import com.intellij.notification.NotificationGroupManager
import com.intellij.notification.NotificationType
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.fileTypes.FileTypeManager
import com.intellij.openapi.progress.ProgressIndicator
import com.intellij.openapi.progress.ProgressManager
import com.intellij.openapi.progress.Task
import com.intellij.openapi.project.Project
import com.intellij.openapi.project.ProjectManager
import com.intellij.openapi.ui.DialogWrapper
import com.intellij.openapi.ui.MessageDialogBuilder
import com.intellij.openapi.ui.Messages
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

object AzureCheck {
    private const val TITLE = "Check in Azure DevOps"

    fun run(project: Project) {
        val settings = AtnSettings.getInstance().state
        val service = AtnService.getInstance(project)
        if (!settings.azureEnabled) {
            val on = MessageDialogBuilder.yesNo("Check your changes in Azure DevOps?",
                "Azure DevOps compiles each pipeline your changes reach, at the base and with your changes, without running it.\n\n" +
                    "Your changed YAML files are pushed to a refs/atn/preview/… ref of their repository first (no branch, no CI run), " +
                    "and you confirm each check. Signs in with the Azure CLI.")
                .yesText("Turn On").noText("Cancel").ask(project)
            if (!on) return
            settings.azureEnabled = true
            // the setting is per IDE: tell the other open projects' servers too
            ProjectManager.getInstance().openProjects.filter { it != project && !it.isDisposed }.forEach { AtnService.getInstance(it).pushSettings() }
        }
        ProgressManager.getInstance().run(object : Task.Backgroundable(project, "Azure DevOps: working out what to check…", true) {
            override fun run(indicator: ProgressIndicator) {
                // the server handles messages in order, so it sees the setting before the plan request
                service.pushSettings().get(30, TimeUnit.SECONDS)
                val plan = service.azurePlan().get(3, TimeUnit.MINUTES).obj()
                ApplicationManager.getApplication().invokeLater { if (!project.isDisposed) confirm(project, plan) }
            }
        })
    }

    private fun confirm(project: Project, plan: JsonObject?) {
        if (plan == null) return
        val error = plan.str("error")
        if (error.isNotEmpty()) {
            Messages.showErrorDialog(project, "Can't check in Azure DevOps: $error", TITLE)
            return
        }
        val pipelines = plan.list("pipelines")
        val skipped = pipelines.filter { it.str("skip").isNotEmpty() }.map { "${it.str("relativePath")}: ${it.str("skip")}" }
        val checkable = plan.int("checkable")
        if (checkable == 0) {
            Messages.showInfoMessage(project, "Nothing to check in Azure DevOps." + if (skipped.isEmpty()) "" else "\n\n" + skipped.joinToString("\n"), TITLE)
            return
        }
        val props = PropertiesComponent.getInstance(project)
        // pushing to GitHub (with the user's own credentials) asks again, even
        // after "Don't ask again" for Azure Repos pushes
        val github = plan.list("snapshots").any { it.get("github")?.asBoolean == true }
        val consentKey = "azureTemplatesNavigator.azureConsent.${plan.str("organization")}" + if (github) ".github" else ""
        if (!props.getBoolean(consentKey, false)) {
            val text = buildString {
                appendLine(plan.str("summary"))
                for (s in plan.list("snapshots")) {
                    appendLine()
                    val where = if (s.get("github")?.asBoolean == true) " (GitHub, with your own git credentials)" else ""
                    appendLine("Pushed to ${s.str("ref")} in ${s.str("repo")}$where:")
                    s.list("files").forEach { appendLine("    ${it.str("status")} ${it.str("relativePath")}") }
                    if (s.get("unpushedBase")?.asBoolean == true) appendLine("    (the base commit isn't on the server yet: its history is uploaded too)")
                }
                appendLine()
                appendLine("Checked:")
                pipelines.filter { it.str("skip").isEmpty() }.forEach {
                    val d = it.get("definition").obj()
                    appendLine("    ${it.str("relativePath")} with pipeline #${d?.int("id")} ${d?.str("name")}")
                }
                if (skipped.isNotEmpty()) {
                    appendLine()
                    appendLine("Not checked:")
                    skipped.forEach { appendLine("    $it") }
                }
                appendLine()
                append("Signed in to ${plan.str("organization")} as ${plan.str("user")}.")
            }
            var always = false
            val ok = MessageDialogBuilder.yesNo("Check $checkable pipeline${if (checkable == 1) "" else "s"} in Azure DevOps?", text)
                .yesText("Check").noText("Cancel")
                .doNotAsk(object : DialogWrapper.DoNotAskOption.Adapter() {
                    override fun rememberChoice(isSelected: Boolean, exitCode: Int) { always = isSelected }
                    override fun getDoNotShowMessage() = "Don't ask again for this project"
                })
                .ask(project)
            if (!ok) return
            if (always) props.setValue(consentKey, true)
        }
        start(project, plan.str("id"))
    }

    private fun start(project: Project, planId: String) {
        val service = AtnService.getInstance(project)
        // the results are shown under each pipeline in Only My Changes
        PropertiesComponent.getInstance(project).setValue(MY_CHANGES_KEY, true, false)
        service.fireChanged()
        ProgressManager.getInstance().run(object : Task.Backgroundable(project, "Azure DevOps", true) {
            override fun run(indicator: ProgressIndicator) {
                val finished = CountDownLatch(1)
                var last: JsonObject? = null
                service.onAzureProgress = { p ->
                    last = p
                    if (p.get("finished")?.asBoolean == true) finished.countDown()
                    else {
                        indicator.text2 = p.str("message")
                        val total = p.int("total")
                        if (total > 0) {
                            indicator.isIndeterminate = false
                            indicator.fraction = p.int("done").toDouble() / total
                        }
                    }
                }
                try {
                    val res = service.azureValidate(planId).get(2, TimeUnit.MINUTES).obj()
                    if (res == null || res.str("error").isNotEmpty()) {
                        notify(project, "Can't check in Azure DevOps: ${res?.str("error") ?: "the language server is not running"}", NotificationType.ERROR)
                        return
                    }
                    while (!finished.await(200, TimeUnit.MILLISECONDS)) {
                        if (indicator.isCanceled) {
                            service.azureCancel()
                            finished.await(30, TimeUnit.SECONDS)
                            break
                        }
                    }
                    last?.let {
                        notify(project, it.str("message"), if (it.str("state") == "done") NotificationType.INFORMATION else NotificationType.WARNING)
                    }
                } finally {
                    service.onAzureProgress = null
                    service.fireChanged()
                }
            }
        })
    }

    private fun notify(project: Project, text: String, type: NotificationType) {
        NotificationGroupManager.getInstance().getNotificationGroup("Azure Templates Navigator")
            .createNotification(TITLE, text, type).notify(project)
    }

    /** Azure's expansion of a pipeline at the base and with your changes, in the diff viewer. */
    fun showDiff(project: Project, path: String) {
        AtnService.getInstance(project).azureExpansion(path).thenAccept { json ->
            val ex = json.obj()
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed) return@invokeLater
                if (ex == null) {
                    Messages.showInfoMessage(project, "No expansion to compare: check in Azure DevOps again.", TITLE)
                    return@invokeLater
                }
                val yaml = FileTypeManager.getInstance().getFileTypeByExtension("yml")
                val f = DiffContentFactory.getInstance()
                DiffManager.getInstance().showDiff(project, SimpleDiffRequest(
                    "${ex.str("title")} (expanded by Azure DevOps)",
                    f.create(project, ex.str("base"), yaml), f.create(project, ex.str("change"), yaml),
                    "Base", "Your changes"))
            }
        }
    }
}
