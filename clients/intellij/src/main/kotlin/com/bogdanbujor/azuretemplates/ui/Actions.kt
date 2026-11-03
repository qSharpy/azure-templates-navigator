package com.bogdanbujor.azuretemplates.ui

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.arr
import com.bogdanbujor.azuretemplates.int
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.str
import com.google.gson.JsonObject
import com.intellij.icons.AllIcons
import com.intellij.ide.actions.RevealFileAction
import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.actionSystem.CommonDataKeys
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.ui.ColoredListCellRenderer
import com.intellij.ui.SimpleTextAttributes
import java.io.File
import javax.swing.JList

object SearchTemplates {
    fun show(project: Project) {
        AtnService.getInstance(project).templates().thenAccept { data ->
            val items = data.arr().mapNotNull { it.obj() }
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed || items.isEmpty()) return@invokeLater
                JBPopupFactory.getInstance().createPopupChooserBuilder(items)
                    .setTitle("Azure Pipelines Templates")
                    .setNamerForFiltering { "${it.str("label")} ${it.str("relativePath")}" }
                    .setRenderer(object : ColoredListCellRenderer<JsonObject>() {
                        override fun customizeCellRenderer(list: JList<out JsonObject>, value: JsonObject, index: Int, selected: Boolean, hasFocus: Boolean) {
                            icon = if (value.str("kind") == "pipeline") AllIcons.Actions.Execute else AllIcons.FileTypes.Yaml
                            append(value.str("label"))
                            append("  ${value.str("relativePath")}", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                            append("  ${value.int("paramCount")}p · used by ${value.int("callerCount")}", SimpleTextAttributes.GRAYED_SMALL_ATTRIBUTES)
                        }
                    })
                    .setItemChosenCallback { AtnService.getInstance(project).openFile(it.str("filePath")) }
                    .createPopup()
                    .showCenteredInCurrentWindow(project)
            }
        }
    }
}

object SearchParameters {
    fun show(project: Project, path: String, line: Int) {
        val svc = AtnService.getInstance(project)
        svc.parameters(path, line).thenAccept { data ->
            val res = data.obj() ?: return@thenAccept
            val file = res.str("filePath")
            val items = res.get("parameters").arr().mapNotNull { it.obj() }
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed || items.isEmpty()) return@invokeLater
                JBPopupFactory.getInstance().createPopupChooserBuilder(items)
                    .setTitle("Template Parameters — ${File(file).name}")
                    .setNamerForFiltering { it.str("name") }
                    .setRenderer(object : ColoredListCellRenderer<JsonObject>() {
                        override fun customizeCellRenderer(list: JList<out JsonObject>, value: JsonObject, index: Int, selected: Boolean, hasFocus: Boolean) {
                            val required = value.get("required")?.asBoolean == true
                            icon = AllIcons.Nodes.Parameter
                            append(value.str("name"), if (required) SimpleTextAttributes.REGULAR_BOLD_ATTRIBUTES else SimpleTextAttributes.REGULAR_ATTRIBUTES)
                            append("  ${value.str("type")}", SimpleTextAttributes.GRAYED_ATTRIBUTES)
                            val def = value.str("default")
                            append(if (required) "  required" else if (def.isNotEmpty()) "  = $def" else "", SimpleTextAttributes.GRAYED_SMALL_ATTRIBUTES)
                        }
                    })
                    .setItemChosenCallback { svc.openFile(file, it.int("line")) }
                    .createPopup()
                    .showCenteredInCurrentWindow(project)
            }
        }
    }
}

abstract class AtnAction : DumbAwareAction() {
    override fun getActionUpdateThread() = ActionUpdateThread.BGT
    override fun update(e: AnActionEvent) {
        e.presentation.isEnabled = e.project != null
    }
}

class SearchTemplatesAction : AtnAction() {
    override fun actionPerformed(e: AnActionEvent) {
        SearchTemplates.show(e.project ?: return)
    }
}

/** Lists the parameters of the template referenced at the caret (or of the current template). */
class SearchParametersAction : AtnAction() {
    override fun update(e: AnActionEvent) {
        val f = e.getData(CommonDataKeys.VIRTUAL_FILE)
        e.presentation.isEnabledAndVisible = e.project != null && f != null &&
            (f.name.endsWith(".yml", true) || f.name.endsWith(".yaml", true))
    }

    override fun actionPerformed(e: AnActionEvent) {
        val project = e.project ?: return
        val file = e.getData(CommonDataKeys.VIRTUAL_FILE) ?: return
        val line = e.getData(CommonDataKeys.EDITOR)?.caretModel?.logicalPosition?.line ?: -1
        SearchParameters.show(project, file.path, line)
    }
}

class RefreshAction : AtnAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val svc = AtnService.getInstance(e.project ?: return)
        svc.execute("atn.refresh").thenRun { svc.fireChanged() }
    }
}

class FetchAllRepositoriesAction : AtnAction() {
    override fun actionPerformed(e: AnActionEvent) {
        val svc = AtnService.getInstance(e.project ?: return)
        svc.execute("atn.fetchAllRepositories").thenRun { svc.fireChanged() }
    }
}

class OpenCacheFolderAction : AtnAction() {
    override fun actionPerformed(e: AnActionEvent) {
        AtnService.getInstance(e.project ?: return).info().thenAccept { info ->
            val dir = File(info.obj()?.str("cacheDir") ?: return@thenAccept).apply { mkdirs() }
            RevealFileAction.openDirectory(dir)
        }
    }
}

class RestartServerAction : AtnAction() {
    override fun actionPerformed(e: AnActionEvent) {
        AtnService.getInstance(e.project ?: return).restart()
    }
}
