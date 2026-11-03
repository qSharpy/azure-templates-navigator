package com.bogdanbujor.azuretemplates.ui

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.arr
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.str
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import com.intellij.icons.AllIcons
import com.intellij.ide.actions.RevealFileAction
import com.intellij.ide.ui.LafManagerListener
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.actionSystem.DataContext
import com.intellij.openapi.actionSystem.DefaultActionGroup
import com.intellij.openapi.actionSystem.ToggleAction
import com.intellij.openapi.actionSystem.ex.ComboBoxAction
import com.intellij.openapi.project.DumbAware
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.Disposable
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.fileEditor.FileEditorManager
import com.intellij.openapi.fileEditor.FileEditorManagerEvent
import com.intellij.openapi.fileEditor.FileEditorManagerListener
import com.intellij.openapi.ide.CopyPasteManager
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.SimpleToolWindowPanel
import com.intellij.openapi.util.Disposer
import com.intellij.ui.JBColor
import com.intellij.ui.awt.RelativePoint
import com.intellij.ui.components.JBLabel
import com.intellij.ui.jcef.JBCefApp
import com.intellij.ui.jcef.JBCefBrowser
import com.intellij.ui.jcef.JBCefJSQuery
import com.intellij.util.ui.JBUI
import com.intellij.util.ui.UIUtil
import java.awt.Color
import java.awt.MouseInfo
import java.awt.Point
import java.awt.datatransfer.StringSelection
import javax.swing.JComponent
import javax.swing.SwingUtilities

/**
 * Hosts the shared graph webview (shared/webview, bundled under /webview).
 * The page talks to us through window.atnHost.postMessage (JBCefJSQuery);
 * we answer with window.postMessage, exactly like the VS Code host.
 *
 * The page runs in "native chrome" mode here: the toolbar above it and the
 * node context menu are IntelliJ components, and the page only draws the graph.
 */
class GraphPanel(private val project: Project) : SimpleToolWindowPanel(true, true), Disposable {
    private val service = AtnService.getInstance(project)
    private var browser: JBCefBrowser? = null
    private var query: JBCefJSQuery? = null
    private var fileScope = true
    private var depth = 1
    private var fullPath = false
    private var menuTarget: String = ""
    private var activeFile: String? = null
    private var ready = false

    init {
        if (!JBCefApp.isSupported()) {
            setContent(JBLabel("The template graph needs JCEF (embedded Chromium), which is not available in this IDE runtime.").apply { border = JBUI.Borders.empty(12) })
        } else {
            val b = JBCefBrowser()
            Disposer.register(this, b)
            val q = JBCefJSQuery.create(b as com.intellij.ui.jcef.JBCefBrowserBase)
            Disposer.register(this, q)
            q.addHandler { msg -> onMessage(msg); null }
            browser = b
            query = q
            setContent(b.component)
            installToolbar()
            load()
            val bus = project.messageBus.connect(this)
            bus.subscribe(AtnService.TOPIC, AtnService.Listener { if (ready) sendGraph() })
            bus.subscribe(FileEditorManagerListener.FILE_EDITOR_MANAGER, object : FileEditorManagerListener {
                override fun selectionChanged(event: FileEditorManagerEvent) = setFile(event.newFile?.takeIf { it.isInLocalFileSystem }?.path)
            })
            ApplicationManager.getApplication().messageBus.connect(this).subscribe(LafManagerListener.TOPIC, LafManagerListener { load() })
            setFile(FileEditorManager.getInstance(project).selectedFiles.firstOrNull()?.path)
        }
    }

    private fun setFile(path: String?) {
        if (path == null || !(path.endsWith(".yml", true) || path.endsWith(".yaml", true))) return
        val changed = path != activeFile
        activeFile = path
        if (changed && fileScope && ready) sendGraph()
    }

    private fun resource(name: String) =
        javaClass.getResourceAsStream("/webview/$name")?.readBytes()?.toString(Charsets.UTF_8) ?: ""

    private fun css(c: Color) = "rgb(${c.red},${c.green},${c.blue})"

    /** Maps the IDE theme onto the CSS variables the shared page uses. */
    private fun themeCss(): String {
        val bg = UIUtil.getPanelBackground()
        val fg = UIUtil.getLabelForeground()
        val border = JBColor.border()
        val input = UIUtil.getTextFieldBackground()
        val sel = UIUtil.getListSelectionBackground(true)
        val hover = UIUtil.getListSelectionBackground(false)
        val muted = UIUtil.getContextHelpForeground()
        val accent = JBUI.CurrentTheme.Focus.focusColor()
        val btn = JBUI.CurrentTheme.ActionButton.hoverBackground()
        val font = UIUtil.getLabelFont()
        val vars = mapOf(
            "editor-background" to css(bg), "editor-foreground" to css(fg), "foreground" to css(fg),
            "sideBar-background" to css(bg), "panel-border" to css(border),
            "input-background" to css(input), "input-foreground" to css(fg), "input-border" to css(border),
            "input-placeholderForeground" to css(muted), "focusBorder" to css(accent),
            "descriptionForeground" to css(muted), "editorWidget-background" to css(bg),
            "button-background" to css(accent), "button-foreground" to "#ffffff", "button-hoverBackground" to css(accent),
            "button-secondaryBackground" to css(btn), "button-secondaryForeground" to css(fg), "button-secondaryHoverBackground" to css(hover),
            "list-hoverBackground" to css(hover), "list-activeSelectionBackground" to css(sel), "list-activeSelectionForeground" to css(UIUtil.getListSelectionForeground(true)),
            "menu-background" to css(bg), "menu-border" to css(border), "menu-foreground" to css(fg),
            "menu-selectionBackground" to css(sel), "menu-selectionForeground" to css(UIUtil.getListSelectionForeground(true)),
            "font-family" to "'${font.family}', system-ui, sans-serif", "font-size" to "${font.size}px",
        )
        return ":root{" + vars.entries.joinToString(";") { "--vscode-${it.key}:${it.value}" } + "}"
    }

    private fun load() {
        val b = browser ?: return
        val q = query ?: return
        ready = false
        val bridge = "window.atnHost={nativeChrome:true,postMessage:function(m){var request=JSON.stringify(m);${q.inject("request")}}};"
        val html = resource("graph.html")
            .replace("{{HEAD}}", "<style>${themeCss()}</style>\n<script>$bridge</script>")
            .replace("{{SCRIPTS}}", "<script>${resource("d3.min.js")}</script>\n<script>${resource("graph.js")}</script>")
        b.loadHTML(html)
    }

    private fun post(msg: JsonObject) {
        val b = browser ?: return
        val js = "window.postMessage(${msg}, '*');"
        ApplicationManager.getApplication().invokeLater { b.cefBrowser.executeJavaScript(js, b.cefBrowser.url, 0) }
    }

    private fun onMessage(raw: String) {
        val msg = runCatching { JsonParser.parseString(raw).asJsonObject }.getOrNull() ?: return
        when (msg.str("type")) {
            "ready" -> {
                ready = true
                sendGraph()
                sendSearch()
                if (fullPath) post(JsonObject().apply { addProperty("type", "setFullPath"); addProperty("enabled", true) })
            }
            "contextMenu" -> ApplicationManager.getApplication().invokeLater { showNodeMenu(msg.str("filePath"), msg.str("kind")) }
            "setFileScope" -> { fileScope = msg.get("enabled")?.asBoolean ?: true; sendGraph() }
            "setDepth" -> { depth = (msg.get("depth")?.asInt ?: 1).coerceIn(1, 10); sendGraph() }
            "openFile", "openFileBeside" -> service.openFile(msg.str("filePath"))
            "copyPath" -> CopyPasteManager.getInstance().setContents(StringSelection(msg.str("text")))
        }
    }

    private fun sendGraph() {
        val scoped = if (fileScope) activeFile else null
        service.graph(scoped, depth).thenAccept { data ->
            val g = data.obj() ?: return@thenAccept
            post(JsonObject().apply {
                addProperty("type", "graphData")
                add("nodes", g.get("nodes"))
                add("edges", g.get("edges"))
                addProperty("fileScopeEnabled", fileScope)
                if (scoped != null) addProperty("scopedFile", scoped)
                addProperty("graphDepth", depth)
            })
        }
    }

    private fun sendSearch() {
        service.templates().thenAccept { data ->
            val items = com.google.gson.JsonArray()
            data.arr().mapNotNull { it.obj() }.forEach { t ->
                val rel = t.str("relativePath")
                items.add(JsonObject().apply {
                    addProperty("filePath", t.str("filePath"))
                    addProperty("filename", t.str("label"))
                    addProperty("relativePath", rel)
                    addProperty("directory", rel.substringBeforeLast('/', ""))
                })
            }
            post(JsonObject().apply { addProperty("type", "searchData"); add("items", items) })
        }
    }

    // ── native chrome ────────────────────────────────────────────────────

    private fun installToolbar() {
        val group = DefaultActionGroup().apply {
            add(object : ToggleAction("Follow Current File", "Show the open file's callers and callees instead of the whole project", AllIcons.General.Locate), DumbAware {
                override fun getActionUpdateThread() = ActionUpdateThread.EDT
                override fun isSelected(e: AnActionEvent) = fileScope
                override fun setSelected(e: AnActionEvent, state: Boolean) { fileScope = state; sendGraph() }
            })
            add(DepthAction())
            add(object : ToggleAction("Show Full Paths", "Label nodes with project-relative paths instead of file names", AllIcons.Actions.ShowAsTree), DumbAware {
                override fun getActionUpdateThread() = ActionUpdateThread.EDT
                override fun isSelected(e: AnActionEvent) = fullPath
                override fun setSelected(e: AnActionEvent, state: Boolean) {
                    fullPath = state
                    post(JsonObject().apply { addProperty("type", "setFullPath"); addProperty("enabled", state) })
                }
            })
            addSeparator()
            add(object : DumbAwareAction("Fit to Window", null, AllIcons.General.FitContent) {
                override fun actionPerformed(e: AnActionEvent) = post(JsonObject().apply { addProperty("type", "fit") })
            })
            add(object : DumbAwareAction("Search Templates…", "Find a template or pipeline and open it", AllIcons.Actions.Search) {
                override fun actionPerformed(e: AnActionEvent) = SearchTemplates.show(project)
            })
            add(object : DumbAwareAction("Refresh", null, AllIcons.Actions.Refresh) {
                override fun actionPerformed(e: AnActionEvent) { service.execute("atn.refresh").thenRun { sendGraph() } }
            })
        }
        val tb = ActionManager.getInstance().createActionToolbar("AzureTemplatesNavigator.Graph", group, true)
        tb.targetComponent = this
        setToolbar(tb.component)
    }

    /** "Depth: 2 ▾" — how many levels around the current file to draw. */
    private inner class DepthAction : ComboBoxAction(), DumbAware {
        override fun getActionUpdateThread() = ActionUpdateThread.EDT
        override fun update(e: AnActionEvent) {
            e.presentation.text = "Depth: $depth"
            e.presentation.description = "Levels of callers and callees to draw around the current file"
            e.presentation.isEnabled = fileScope
        }

        override fun createPopupActionGroup(button: JComponent, dataContext: DataContext): DefaultActionGroup =
            DefaultActionGroup().apply {
                for (d in 1..10) add(object : DumbAwareAction(d.toString()) {
                    override fun actionPerformed(e: AnActionEvent) { depth = d; sendGraph() }
                })
            }
    }

    private fun showNodeMenu(filePath: String, kind: String) {
        if (filePath.isEmpty()) return
        menuTarget = filePath
        val openable = kind != "missing" && kind != "unknown"
        val group = DefaultActionGroup().apply {
            if (openable) {
                add(object : DumbAwareAction("Open", null, AllIcons.Actions.EditSource) {
                    override fun actionPerformed(e: AnActionEvent) = service.openFile(menuTarget)
                })
                add(object : DumbAwareAction("Show Usages", "Pipelines and templates that reference this file", AllIcons.Actions.Find) {
                    override fun actionPerformed(e: AnActionEvent) = UsagesPopup.show(project, menuTarget, 0, 0, null, pointer())
                })
                add(object : DumbAwareAction("Center Graph on This File") {
                    override fun actionPerformed(e: AnActionEvent) { fileScope = true; activeFile = menuTarget; sendGraph() }
                })
                addSeparator()
            }
            add(object : DumbAwareAction("Copy Path", null, AllIcons.Actions.Copy) {
                override fun actionPerformed(e: AnActionEvent) = CopyPasteManager.getInstance().setContents(StringSelection(menuTarget))
            })
            if (openable) add(object : DumbAwareAction("Reveal in Finder / Explorer") {
                override fun actionPerformed(e: AnActionEvent) = RevealFileAction.openFile(java.io.File(menuTarget))
            })
        }
        val comp = browser?.component ?: return
        val p = pointer()?.getPoint(comp) ?: return
        ActionManager.getInstance().createActionPopupMenu("AzureTemplatesNavigator.GraphNode", group).component.show(comp, p.x, p.y)
    }

    private fun pointer(): RelativePoint? {
        val comp = browser?.component ?: return null
        val screen = MouseInfo.getPointerInfo()?.location ?: return null
        val p = Point(screen)
        SwingUtilities.convertPointFromScreen(p, comp)
        return RelativePoint(comp, p)
    }

    override fun dispose() {}
}
