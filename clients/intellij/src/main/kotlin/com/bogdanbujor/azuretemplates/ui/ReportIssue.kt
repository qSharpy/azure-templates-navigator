package com.bogdanbujor.azuretemplates.ui

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.IssueReport
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.str
import com.intellij.icons.AllIcons
import com.intellij.ide.BrowserUtil
import com.intellij.ide.plugins.PluginManagerCore
import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.application.ApplicationInfo
import com.intellij.openapi.diagnostic.ErrorReportSubmitter
import com.intellij.openapi.diagnostic.IdeaLoggingEvent
import com.intellij.openapi.diagnostic.SubmittedReportInfo
import com.intellij.openapi.extensions.PluginId
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.util.SystemInfo
import com.intellij.ui.ColoredListCellRenderer
import com.intellij.util.Consumer
import java.awt.Component
import java.util.concurrent.CompletableFuture
import java.util.concurrent.TimeUnit
import javax.swing.JList

private const val PLUGIN_ID = "com.bogdanbujor.azure-templates-navigator"

internal object IssueEnvironment {
    fun lines(atnVersion: String?): List<String> {
        val app = ApplicationInfo.getInstance()
        val plugin = PluginManagerCore.getPlugin(PluginId.getId(PLUGIN_ID))?.version ?: "unknown"
        return listOf(
            "Azure Templates Navigator $plugin",
            "atn ${atnVersion ?: "unknown (server not running)"}",
            "${app.fullApplicationName} (build ${app.build.asString()})",
            "${SystemInfo.OS_NAME} ${SystemInfo.OS_VERSION} ${System.getProperty("os.arch")}",
        )
    }

    /** The server's version, or null when it isn't running. Never blocks for long: reports often come when it's broken. */
    fun atnVersion(project: Project?): CompletableFuture<String?> {
        if (project == null || project.isDisposed) return CompletableFuture.completedFuture(null)
        return AtnService.getInstance(project).info()
            .thenApply { it.obj()?.str("version")?.takeIf(String::isNotEmpty) }
            .completeOnTimeout(null, 2, TimeUnit.SECONDS)
            .exceptionally { null }
    }

    fun open(project: Project?, kind: IssueReport.Kind) {
        atnVersion(project).thenAccept { atn -> BrowserUtil.browse(IssueReport.url(kind, lines(atn))) }
    }
}

/** Help | Report Azure Templates Navigator Issue…: a bug report or a feature request, pre-filled with versions. */
class ReportIssueAction : DumbAwareAction() {
    override fun getActionUpdateThread() = ActionUpdateThread.BGT

    override fun actionPerformed(e: AnActionEvent) {
        val project = e.project
        val choices = listOf(
            IssueReport.Kind.BUG to "Report a bug",
            IssueReport.Kind.FEATURE to "Suggest a feature or give feedback",
        )
        val popup = JBPopupFactory.getInstance().createPopupChooserBuilder(choices)
            .setTitle("Opens a New Issue on GitHub")
            .setRenderer(object : ColoredListCellRenderer<Pair<IssueReport.Kind, String>>() {
                override fun customizeCellRenderer(list: JList<out Pair<IssueReport.Kind, String>>, value: Pair<IssueReport.Kind, String>, index: Int, selected: Boolean, hasFocus: Boolean) {
                    icon = if (value.first == IssueReport.Kind.BUG) AllIcons.Actions.StartDebugger else AllIcons.Actions.IntentionBulb
                    append(value.second)
                }
            })
            .setItemChosenCallback { IssueEnvironment.open(project, it.first) }
            .createPopup()
        if (project != null) popup.showCenteredInCurrentWindow(project) else popup.showInFocusCenter()
    }
}

/**
 * Adds "Report to Azure Templates Navigator" to IntelliJ's error dialog for
 * exceptions thrown by this plugin. It opens a pre-filled GitHub issue with the
 * stack trace; nothing is sent until the user submits it there.
 */
class AtnErrorReportSubmitter : ErrorReportSubmitter() {
    override fun getReportActionText() = "Report to Azure Templates Navigator"

    override fun submit(
        events: Array<out IdeaLoggingEvent>,
        additionalInfo: String?,
        parentComponent: Component,
        consumer: Consumer<in SubmittedReportInfo>,
    ): Boolean {
        val event = events.firstOrNull() ?: return false
        val home = System.getProperty("user.home")
        val firstLine = (event.throwable?.let { "${it.javaClass.simpleName}: ${it.message ?: ""}" } ?: event.message ?: "Error")
            .lineSequence().first().take(120)
        val logs = listOfNotNull(additionalInfo?.takeIf { it.isNotBlank() }, event.throwableText)
            .joinToString("\n\n")
            .let { if (home.isNullOrEmpty()) it else it.replace(home, "~") }
        val url = IssueReport.url(IssueReport.Kind.BUG, IssueEnvironment.lines(null), title = "Error: ${firstLine.replace(home ?: "\u0000", "~")}", logs = logs)
        BrowserUtil.browse(url)
        consumer.consume(SubmittedReportInfo(SubmittedReportInfo.SubmissionStatus.NEW_ISSUE))
        return true
    }
}
