package com.bogdanbujor.azuretemplates.ui

// Native IntelliJ navigation on top of the server's results:
//   - a usages popup (Code Vision "N usages", gutter icons, the Tools menu)
//   - gutter icons on `template:` references
//   - the Structure view for pipeline files (LSP document symbols)
// No YAML or template logic lives here: what a usage is, and where a
// reference points, is decided by the server.

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.int
import com.bogdanbujor.azuretemplates.list
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.str
import com.google.gson.JsonObject
import com.intellij.codeInsight.daemon.GutterIconNavigationHandler
import com.intellij.codeInsight.daemon.LineMarkerInfo
import com.intellij.codeInsight.daemon.LineMarkerProvider
import com.intellij.codeInsight.hint.HintManager
import com.intellij.icons.AllIcons
import com.intellij.ide.structureView.StructureViewBuilder
import com.intellij.lang.PsiStructureViewFactory
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.actionSystem.CommonDataKeys
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.editor.Editor
import com.intellij.openapi.editor.markup.GutterIconRenderer
import com.intellij.openapi.fileEditor.FileDocumentManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.MessageType
import com.intellij.openapi.ui.popup.JBPopup
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.wm.WindowManager
import com.intellij.psi.PsiElement
import com.intellij.psi.PsiFile
import com.intellij.ui.ColoredListCellRenderer
import com.intellij.ui.SimpleTextAttributes
import com.intellij.ui.awt.RelativePoint
import com.redhat.devtools.lsp4ij.commands.CommandExecutor
import com.redhat.devtools.lsp4ij.features.documentSymbol.LSPDocumentSymbolStructureViewFactory
import org.jetbrains.yaml.YAMLTokenTypes
import org.jetbrains.yaml.psi.YAMLKeyValue
import org.jetbrains.yaml.psi.YAMLScalar
import org.jetbrains.yaml.structureView.YAMLStructureViewFactory
import java.awt.event.MouseEvent
import java.io.File
import java.net.URI
import java.nio.file.Paths
import javax.swing.JList

object UsagesPopup {
    /**
     * Asks the server for the usages of whatever is at [line]:[character] of
     * [path] and shows them in a chooser popup, next to [editor] or [at].
     * A single usage is opened directly, like IntelliJ's own Show Usages.
     */
    fun show(project: Project, path: String, line: Int, character: Int, editor: Editor?, at: RelativePoint? = null) {
        val svc = AtnService.getInstance(project)
        svc.references(path, line, character).thenAccept { data ->
            val res = data.obj()
            val usages = res?.list("usages").orEmpty()
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed) return@invokeLater
                when {
                    usages.isEmpty() -> notify(project, editor, at, "No usages found in this project")
                    usages.size == 1 -> open(svc, usages[0])
                    else -> {
                        val popup = JBPopupFactory.getInstance().createPopupChooserBuilder(usages)
                            .setTitle(res?.str("title") ?: "Usages")
                            .setNamerForFiltering { "${it.str("relativePath")} ${it.str("text")}" }
                            .setRenderer(UsageRenderer())
                            .setItemChosenCallback { open(svc, it) }
                            .createPopup()
                        showPopup(popup, project, editor, at)
                    }
                }
            }
        }
    }

    private fun open(svc: AtnService, u: JsonObject) = svc.openFile(u.str("path"), u.int("line"), u.int("col"))

    private fun showPopup(popup: JBPopup, project: Project, editor: Editor?, at: RelativePoint?) = when {
        at != null -> popup.show(at)
        editor != null -> popup.showInBestPositionFor(editor)
        else -> popup.showCenteredInCurrentWindow(project)
    }

    private fun notify(project: Project, editor: Editor?, at: RelativePoint?, text: String) {
        if (editor != null && at == null) {
            HintManager.getInstance().showInformationHint(editor, text)
            return
        }
        val point = at ?: WindowManager.getInstance().getFrame(project)?.let { RelativePoint.getCenterOf(it.rootPane) } ?: return
        JBPopupFactory.getInstance().createHtmlTextBalloonBuilder(text, MessageType.INFO, null)
            .setFadeoutTime(2500).createBalloon().show(point, com.intellij.openapi.ui.popup.Balloon.Position.above)
    }

    /** "build.yml:12   - template: build.yml@platform        teams/search" */
    private class UsageRenderer : ColoredListCellRenderer<JsonObject>() {
        override fun customizeCellRenderer(list: JList<out JsonObject>, value: JsonObject, index: Int, selected: Boolean, hasFocus: Boolean) {
            val rel = value.str("relativePath")
            icon = if (value.str("kind") == "pipeline") AllIcons.Actions.Execute else AllIcons.FileTypes.Yaml
            append(File(rel).name, SimpleTextAttributes.REGULAR_ATTRIBUTES)
            append(":${value.int("line") + 1}", SimpleTextAttributes.GRAYED_ATTRIBUTES)
            append("   ${value.str("text")}", SimpleTextAttributes.REGULAR_ATTRIBUTES)
            val dir = File(rel).parent
            if (!dir.isNullOrEmpty()) append("   $dir", SimpleTextAttributes.GRAYED_SMALL_ATTRIBUTES)
        }
    }
}

/**
 * "Show Template Usages". Run by Code Vision when the "N usages" lens above a
 * template is clicked (LSP4IJ dispatches the lens command
 * `AzureTemplatesNavigator.showUsages` to this action), or from the menu for
 * the element at the caret.
 */
class ShowUsagesAction : AtnAction() {
    override fun update(e: AnActionEvent) {
        val f = e.getData(CommonDataKeys.VIRTUAL_FILE)
        e.presentation.isEnabledAndVisible = e.project != null && (e.getData(CommandExecutor.LSP_COMMAND) != null ||
            (f != null && (f.name.endsWith(".yml", true) || f.name.endsWith(".yaml", true))))
    }

    override fun actionPerformed(e: AnActionEvent) {
        val project = e.project ?: return
        val editor = e.getData(CommonDataKeys.EDITOR)
        val cmd = e.getData(CommandExecutor.LSP_COMMAND)
        if (cmd != null) {
            val arg = runCatching { AtnService.gson.toJsonTree(cmd.getArgumentAt(0)).asJsonObject }.getOrNull() ?: return
            val path = runCatching { Paths.get(URI(arg.str("uri"))).toString() }.getOrNull() ?: return
            UsagesPopup.show(project, path, arg.int("line"), arg.int("character"), editor, mousePoint(e))
            return
        }
        val file = e.getData(CommonDataKeys.VIRTUAL_FILE) ?: return
        val pos = editor?.caretModel?.logicalPosition
        UsagesPopup.show(project, file.path, pos?.line ?: 0, pos?.column ?: 0, editor)
    }

    private fun mousePoint(e: AnActionEvent): RelativePoint? = (e.inputEvent as? MouseEvent)?.let { RelativePoint(it) }
}

/**
 * Gutter icon on every `template:` reference (including `extends: template:`):
 * the "implements" arrow, clicking it opens the referenced template.
 */
class TemplateLineMarkerProvider : LineMarkerProvider {
    override fun getLineMarkerInfo(element: PsiElement): LineMarkerInfo<*>? {
        if (element.node?.elementType != YAMLTokenTypes.SCALAR_KEY || element.text != "template") return null
        val kv = element.parent as? YAMLKeyValue ?: return null
        val value = (kv.value as? YAMLScalar)?.textValue ?: return null
        if (value.isBlank() || value.contains("\${{")) return null // resolved at run time
        return LineMarkerInfo(
            element, element.textRange, AllIcons.Gutter.ImplementingMethod,
            { "Go to template $value" },
            GutterIconNavigationHandler<PsiElement> { e, elt -> navigate(e, elt) },
            GutterIconRenderer.Alignment.RIGHT,
            { "Go to template" },
        )
    }

    private fun navigate(e: MouseEvent, elt: PsiElement) {
        val project = elt.project
        val vf = elt.containingFile?.virtualFile ?: return
        val doc = FileDocumentManager.getInstance().getDocument(vf) ?: return
        val line = doc.getLineNumber(elt.textOffset)
        val point = RelativePoint(e)
        val svc = AtnService.getInstance(project)
        svc.parameters(vf.path, line).thenAccept { data ->
            val target = data.obj()?.str("filePath").orEmpty()
            ApplicationManager.getApplication().invokeLater {
                if (target.isEmpty() || target == vf.path) {
                    JBPopupFactory.getInstance().createHtmlTextBalloonBuilder("Template not found — see the problem on this line", MessageType.WARNING, null)
                        .setFadeoutTime(2500).createBalloon().show(point, com.intellij.openapi.ui.popup.Balloon.Position.above)
                } else {
                    svc.openFile(target)
                }
            }
        }
    }
}

/**
 * Structure view (Cmd/Ctrl+F12, Structure tool window): Azure Pipelines files
 * get the server's outline (stages → jobs → steps, template calls,
 * parameters, ${{ if }} blocks); any other YAML keeps the YAML plugin's view.
 */
class AtnStructureViewFactory : PsiStructureViewFactory {
    private val lsp = LSPDocumentSymbolStructureViewFactory()
    private val yaml = YAMLStructureViewFactory()

    override fun getStructureViewBuilder(file: PsiFile): StructureViewBuilder? =
        if (looksLikePipeline(file.text)) lsp.getStructureViewBuilder(file) ?: yaml.getStructureViewBuilder(file)
        else yaml.getStructureViewBuilder(file)

    companion object {
        private val pipelineKey = Regex("(?m)^(stages|jobs|steps|extends|trigger|pr|schedules|resources)\\s*:")
        private val githubActions = Regex("(?m)^on\\s*:")

        /** Which outline to show; the server analyses every YAML file regardless. */
        fun looksLikePipeline(text: String) =
            (pipelineKey.containsMatchIn(text) && !githubActions.containsMatchIn(text)) || text.contains("\${{ parameters")
    }
}
