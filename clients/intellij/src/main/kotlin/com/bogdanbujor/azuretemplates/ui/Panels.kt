package com.bogdanbujor.azuretemplates.ui

// Tool window tabs. Each tab renders view models computed by the server
// (atn/dependencyTree, atn/workspaceDiagnostics, atn/repositories, atn/stats), so the
// content is identical to the VS Code views.

import com.bogdanbujor.azuretemplates.AtnService
import com.bogdanbujor.azuretemplates.arr
import com.bogdanbujor.azuretemplates.int
import com.bogdanbujor.azuretemplates.list
import com.bogdanbujor.azuretemplates.obj
import com.bogdanbujor.azuretemplates.str
import com.google.gson.JsonElement
import com.google.gson.JsonObject
import com.intellij.icons.AllIcons
import com.intellij.ide.actions.RevealFileAction
import com.intellij.openapi.Disposable
import com.intellij.openapi.diagnostic.ControlFlowException
import com.intellij.openapi.diagnostic.logger
import com.intellij.openapi.util.text.StringUtil
import com.intellij.openapi.actionSystem.ActionManager
import com.intellij.openapi.actionSystem.ActionUpdateThread
import com.intellij.openapi.actionSystem.AnAction
import com.intellij.openapi.actionSystem.AnActionEvent
import com.intellij.openapi.actionSystem.DefaultActionGroup
import com.intellij.openapi.application.ApplicationManager
import com.intellij.openapi.fileEditor.FileEditorManager
import com.intellij.openapi.fileEditor.FileEditorManagerEvent
import com.intellij.openapi.fileEditor.FileEditorManagerListener
import com.intellij.openapi.project.DumbAwareAction
import com.intellij.openapi.project.Project
import com.intellij.openapi.ui.SimpleToolWindowPanel
import com.intellij.openapi.util.Disposer
import com.intellij.openapi.vfs.VirtualFile
import com.intellij.openapi.wm.ToolWindow
import com.intellij.openapi.wm.ToolWindowFactory
import com.intellij.ui.ColoredTreeCellRenderer
import com.intellij.ui.PopupHandler
import com.intellij.ui.ScrollPaneFactory
import com.intellij.ui.JBColor
import com.intellij.ui.SimpleTextAttributes
import com.intellij.ui.components.JBLabel
import com.intellij.ui.content.ContentFactory
import com.intellij.ui.treeStructure.Tree
import com.intellij.util.ui.JBUI
import com.intellij.util.ui.WrapLayout
import com.intellij.util.ui.tree.TreeUtil
import java.awt.BorderLayout
import java.awt.CardLayout
import java.awt.FlowLayout
import java.awt.Graphics
import java.awt.Graphics2D
import java.awt.RenderingHints
import java.awt.event.FocusAdapter
import java.awt.event.FocusEvent
import java.awt.event.KeyAdapter
import java.awt.event.KeyEvent
import java.awt.event.MouseAdapter
import java.awt.event.MouseEvent
import java.io.File
import java.util.concurrent.CompletableFuture
import javax.swing.Icon
import javax.swing.JComponent
import javax.swing.JPanel
import javax.swing.JTree
import javax.swing.SwingConstants
import javax.swing.tree.DefaultMutableTreeNode
import javax.swing.tree.DefaultTreeModel

class AtnToolWindowFactory : ToolWindowFactory {
    override fun createToolWindowContent(project: Project, toolWindow: ToolWindow) {
        // A bug button in the tool window header, visible on every tab. Set first,
        // so it is there even when a tab fails.
        toolWindow.setTitleActions(listOfNotNull(ActionManager.getInstance().getAction("AzureTemplatesNavigator.ReportIssue")))
        val tabs = AtnTabs()
        // Each tab is built on its own: one that throws (JCEF unavailable, an API
        // changed in a newer IDE) shows its error instead of hiding every tab after it.
        fun add(title: String, create: () -> SimpleToolWindowPanel) {
            val panel = try {
                create()
            } catch (e: Throwable) {
                if (e is ControlFlowException) throw e
                LOG.error("[atn] the $title tab could not be created", e)
                FailedTabPanel(title, e)
            }
            tabs.addTab(title, panel)
        }
        add("Dependencies") { DependenciesPanel(project) }
        add("Graph") { GraphPanel(project) }
        add("Diagnostics") { DiagnosticsPanel(project) }
        add("Repositories") { RepositoriesPanel(project) }
        add("Stats") { StatsPanel(project) }
        val content = ContentFactory.getInstance().createContent(tabs, "", false)
        content.setDisposer(tabs)
        toolWindow.contentManager.addContent(content)
    }

    companion object {
        private val LOG = logger<AtnToolWindowFactory>()
    }
}

/**
 * The tool window's tabs. They live in the panel rather than in the tool window
 * header: header tabs that don't fit are hidden behind a dropdown, while this
 * strip wraps onto another row, so every tab stays one click away at any width.
 * The tabs keep their order when wrapped (a JTabbedPane moves the selected row
 * next to the content, so rows swap on every click).
 */
class AtnTabs : JPanel(BorderLayout()), Disposable {
    private val strip = JPanel(WrapLayout(FlowLayout.LEFT, JBUI.scale(2), JBUI.scale(2))).apply {
        isOpaque = false
        border = JBUI.Borders.compound(
            JBUI.Borders.customLine(JBColor.border(), 0, 0, 1, 0),
            JBUI.Borders.empty(2, 4),
        )
    }
    private val cards = CardLayout()
    private val body = JPanel(cards)
    private val tabs = mutableListOf<Pair<TabButton, JComponent>>()

    init {
        add(strip, BorderLayout.NORTH)
        add(body, BorderLayout.CENTER)
    }

    fun addTab(title: String, panel: JComponent) {
        val key = tabs.size.toString()
        val button = TabButton(title) { selectIndex(tabs.indexOfFirst { it.second === panel }) }
        tabs += button to panel
        strip.add(button)
        body.add(panel, key)
        if (panel is Disposable) Disposer.register(this, panel)
        if (tabs.size == 1) selectIndex(0)
    }

    private fun selectIndex(i: Int) {
        if (i !in tabs.indices) return
        tabs.forEachIndexed { j, (b, _) -> b.selected = j == i }
        cards.show(body, i.toString())
    }

    /** Selects the first tab whose panel is a [T] and returns that panel. */
    inline fun <reified T : JComponent> select(): T? = select(T::class.java)

    fun <T : JComponent> select(type: Class<T>): T? {
        val i = tabs.indexOfFirst { type.isInstance(it.second) }
        if (i < 0) return null
        selectIndex(i)
        return type.cast(tabs[i].second)
    }

    override fun dispose() {}

    /** One tab: plain text, a rounded highlight when selected or hovered. Focusable; Enter or Space selects it. */
    private class TabButton(text: String, private val onSelect: () -> Unit) : JBLabel(text) {
        var selected = false
            set(v) { field = v; repaint() }
        private var hover = false

        init {
            border = JBUI.Borders.empty(4, 10)
            isFocusable = true
            getAccessibleContext().accessibleName = text
            addMouseListener(object : MouseAdapter() {
                override fun mousePressed(e: MouseEvent) { onSelect(); requestFocusInWindow() }
                override fun mouseEntered(e: MouseEvent) { hover = true; repaint() }
                override fun mouseExited(e: MouseEvent) { hover = false; repaint() }
            })
            addKeyListener(object : KeyAdapter() {
                override fun keyPressed(e: KeyEvent) {
                    if (e.keyCode == KeyEvent.VK_ENTER || e.keyCode == KeyEvent.VK_SPACE) onSelect()
                }
            })
            addFocusListener(object : FocusAdapter() {
                override fun focusGained(e: FocusEvent) = repaint()
                override fun focusLost(e: FocusEvent) = repaint()
            })
        }

        override fun paintComponent(g: Graphics) {
            val bg = when {
                selected -> JBUI.CurrentTheme.ActionButton.pressedBackground()
                hover -> JBUI.CurrentTheme.ActionButton.hoverBackground()
                else -> null
            }
            val g2 = g.create() as Graphics2D
            try {
                g2.setRenderingHint(RenderingHints.KEY_ANTIALIASING, RenderingHints.VALUE_ANTIALIAS_ON)
                val arc = JBUI.scale(8)
                if (bg != null) {
                    g2.color = bg
                    g2.fillRoundRect(0, 0, width, height, arc, arc)
                }
                if (isFocusOwner) {
                    g2.color = JBUI.CurrentTheme.Focus.focusColor()
                    g2.drawRoundRect(0, 0, width - 1, height - 1, arc, arc)
                }
            } finally {
                g2.dispose()
            }
            super.paintComponent(g)
        }
    }
}

/** Shown in place of a tab whose panel threw while being created. */
class FailedTabPanel(title: String, error: Throwable) : SimpleToolWindowPanel(true, true) {
    init {
        val reason = (error.message ?: error.javaClass.simpleName).let { StringUtil.escapeXmlEntities(it) }
        setContent(JBLabel(
            "<html>The $title tab could not be created:<br><br><code>${error.javaClass.name}: $reason</code><br><br>" +
                "Please report it with the bug button above (the IDE log has the full stack trace: Help → Show Log).</html>"
        ).apply {
            border = JBUI.Borders.empty(12)
            verticalAlignment = SwingConstants.TOP
        })
    }
}

/** A row in one of the trees. */
data class Item(
    val label: String,
    val description: String = "",
    val icon: Icon? = null,
    val tooltip: String? = null,
    val error: Boolean = false,
    val data: JsonObject? = null,
    val open: (() -> Unit)? = null,
)

/** Base class: a tree fed by an async server request, refreshed on change. */
abstract class ServerTreePanel(protected val project: Project) : SimpleToolWindowPanel(true, true), Disposable {
    protected val service = AtnService.getInstance(project)
    protected val tree = Tree(DefaultTreeModel(DefaultMutableTreeNode()))

    init {
        tree.isRootVisible = false
        tree.showsRootHandles = true
        tree.cellRenderer = object : ColoredTreeCellRenderer() {
            override fun customizeCellRenderer(t: JTree, value: Any?, selected: Boolean, expanded: Boolean, leaf: Boolean, row: Int, hasFocus: Boolean) {
                val item = (value as? DefaultMutableTreeNode)?.userObject as? Item ?: return
                icon = item.icon
                append(item.label, if (item.error) SimpleTextAttributes.ERROR_ATTRIBUTES else SimpleTextAttributes.REGULAR_ATTRIBUTES)
                if (item.description.isNotEmpty()) append("  " + item.description, SimpleTextAttributes.GRAYED_ATTRIBUTES)
                toolTipText = item.tooltip
            }
        }
        tree.addMouseListener(object : MouseAdapter() {
            override fun mouseClicked(e: MouseEvent) {
                if (e.clickCount == 2) selectedItem()?.open?.invoke()
            }
        })
        tree.addKeyListener(object : KeyAdapter() {
            override fun keyPressed(e: KeyEvent) {
                if (e.keyCode == KeyEvent.VK_ENTER) selectedItem()?.open?.invoke()
            }
        })
        setContent(ScrollPaneFactory.createScrollPane(tree))
        project.messageBus.connect(this).subscribe(AtnService.TOPIC, AtnService.Listener { reload() })
    }

    protected fun selectedItem(): Item? =
        (tree.lastSelectedPathComponent as? DefaultMutableTreeNode)?.userObject as? Item

    protected fun toolbar(vararg actions: AnAction) {
        val group = DefaultActionGroup(*actions)
        val tb = ActionManager.getInstance().createActionToolbar("AzureTemplatesNavigator", group, true)
        tb.targetComponent = this
        setToolbar(tb.component)
    }

    protected fun contextMenu(vararg actions: AnAction) {
        PopupHandler.installPopupMenu(tree, DefaultActionGroup(*actions), "AzureTemplatesNavigatorPopup")
    }

    abstract fun load(): CompletableFuture<JsonElement?>
    abstract fun build(root: DefaultMutableTreeNode, json: JsonElement?)

    fun reload() {
        load().thenAccept { json ->
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed) return@invokeLater
                val expanded = TreeUtil.collectExpandedUserObjects(tree).map { (it as? Item)?.label }.toSet()
                val root = DefaultMutableTreeNode()
                build(root, json)
                (tree.model as DefaultTreeModel).setRoot(root)
                TreeUtil.treeNodeTraverser(root).forEach { n ->
                    val node = n as DefaultMutableTreeNode
                    val item = node.userObject as? Item
                    if (node.level == 1 || (item != null && item.label in expanded)) {
                        tree.expandPath(TreeUtil.getPathFromRoot(node))
                    }
                }
            }
        }
    }

    protected fun action(text: String, icon: Icon, run: () -> Unit) = object : DumbAwareAction(text, null, icon) {
        override fun actionPerformed(e: AnActionEvent) = run()
        override fun getActionUpdateThread() = ActionUpdateThread.BGT
    }

    override fun dispose() {}
}

private fun kindIcon(kind: String, severity: Int): Icon = when {
    severity == 1 || kind == "missing" || kind == "unknownAlias" -> AllIcons.General.Error
    kind == "repoUnavailable" -> AllIcons.Actions.Download
    severity == 2 -> AllIcons.General.Warning
    kind == "pipeline" -> AllIcons.Actions.Execute
    kind == "external" -> AllIcons.Nodes.Module
    kind == "dynamic" -> AllIcons.Nodes.Variable
    kind == "cycle" -> AllIcons.Actions.Refresh
    else -> AllIcons.FileTypes.Yaml
}

// ── Dependencies ──────────────────────────────────────────────────────────
class DependenciesPanel(project: Project) : ServerTreePanel(project) {
    private var file: String? = null

    init {
        toolbar(
            action("Refresh", AllIcons.Actions.Refresh) { service.execute("atn.refresh"); reload() },
            action("Search Templates…", AllIcons.Actions.Search) { SearchTemplates.show(project) },
            action("Expand All", AllIcons.Actions.Expandall) { TreeUtil.expandAll(tree) },
            action("Collapse All", AllIcons.Actions.Collapseall) { TreeUtil.collapseAll(tree, 1) },
        )
        contextMenu(
            action("Open", AllIcons.Actions.EditSource) { selectedItem()?.open?.invoke() },
            action("Go to Reference", AllIcons.Actions.Find) {
                selectedItem()?.data?.let { service.openFile(it.str("refFile"), it.int("refLine")) }
            },
            action("Fetch Repository", AllIcons.Actions.Download) {
                selectedItem()?.data?.let { if (it.str("alias").isNotEmpty()) service.fetchRepository(it.str("alias"), it.str("refFile")) }
            },
        )
        project.messageBus.connect(this).subscribe(FileEditorManagerListener.FILE_EDITOR_MANAGER, object : FileEditorManagerListener {
            override fun selectionChanged(event: FileEditorManagerEvent) = setFile(event.newFile)
        })
        setFile(FileEditorManager.getInstance(project).selectedFiles.firstOrNull())
    }

    private fun setFile(vf: VirtualFile?) {
        if (vf == null || vf.extension?.lowercase() !in setOf("yml", "yaml") || !vf.isInLocalFileSystem) return
        file = vf.path
        reload()
    }

    override fun load(): CompletableFuture<JsonElement?> =
        file?.let { service.dependencyTree(it) } ?: CompletableFuture.completedFuture(null)

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        val t = json.obj() ?: run {
            root.add(DefaultMutableTreeNode(Item("Open a pipeline or template YAML file")))
            return
        }
        val isCalling = t.list("isCalling")
        val calledBy = t.list("calledBy")
        val header = DefaultMutableTreeNode(Item(t.str("label"), t.str("relativePath"), kindIcon(t.str("kind"), t.int("severity"))))
        root.add(header)
        val down = DefaultMutableTreeNode(Item("Is calling (${isCalling.size})", icon = AllIcons.General.ArrowDown))
        isCalling.forEach { down.add(node(it)) }
        val up = DefaultMutableTreeNode(Item("Called by (${t.int("callerCount")})", icon = AllIcons.General.ArrowUp))
        calledBy.forEach { up.add(node(it)) }
        root.add(down)
        root.add(up)
    }

    private fun node(n: JsonObject): DefaultMutableTreeNode {
        val kind = n.str("kind")
        val filePath = n.str("filePath")
        val isFile = filePath.isNotEmpty() && kind != "missing"
        val desc = listOf(n.str("description"), if (n.int("paramCount") > 0) "${n.int("paramCount")}p" else "").filter { it.isNotEmpty() }.joinToString(" · ")
        val open: () -> Unit = when {
            isFile -> { { service.openFile(filePath, n.int("line")) } }
            kind == "repoUnavailable" -> { { service.fetchRepository(n.str("alias"), n.str("refFile")) } }
            else -> { { service.openFile(n.str("refFile"), n.int("refLine")) } }
        }
        val node = DefaultMutableTreeNode(Item(n.str("label"), desc, kindIcon(kind, n.int("severity")), n.str("tooltip"),
            error = kind == "missing" || kind == "unknownAlias", data = n, open = open))
        n.list("children").forEach { node.add(node(it)) }
        return node
    }
}

// ── Diagnostics ───────────────────────────────────────────────────────────
class DiagnosticsPanel(project: Project) : ServerTreePanel(project) {
    init {
        toolbar(action("Refresh", AllIcons.Actions.Refresh) { service.execute("atn.refresh"); reload() })
        reload()
    }

    override fun load() = service.workspaceDiagnostics()

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        val files = json.arr().mapNotNull { it.obj() }
        if (files.isEmpty()) {
            root.add(DefaultMutableTreeNode(Item("No problems found", icon = AllIcons.General.InspectionsOK)))
            return
        }
        for (f in files) {
            val counts = listOfNotNull(
                f.int("errors").takeIf { it > 0 }?.let { "$it error(s)" },
                f.int("warnings").takeIf { it > 0 }?.let { "$it warning(s)" },
            ).joinToString(", ")
            val path = f.str("filePath")
            val fileNode = DefaultMutableTreeNode(Item(f.str("relativePath"), counts, AllIcons.FileTypes.Yaml, open = { service.openFile(path) }))
            for (d in f.list("diagnostics")) {
                val err = d.int("severity") == 1
                fileNode.add(DefaultMutableTreeNode(Item(d.str("message"), "Ln ${d.int("line") + 1}",
                    if (err) AllIcons.General.Error else AllIcons.General.Warning, "[${d.str("code")}]",
                    open = { service.openFile(path, d.int("line")) })))
            }
            root.add(fileNode)
        }
    }
}

// ── Repositories ──────────────────────────────────────────────────────────
class RepositoriesPanel(project: Project) : ServerTreePanel(project) {
    init {
        toolbar(
            action("Update All Downloaded Repositories", AllIcons.Actions.Refresh) {
                service.execute("atn.fetchAllRepositories").thenRun { service.fireChanged() }
            },
            action("Open Cache Folder", AllIcons.Actions.MenuOpen) {
                service.info().thenAccept { info ->
                    val dir = File(info.obj()?.str("cacheDir") ?: return@thenAccept).apply { mkdirs() }
                    RevealFileAction.openDirectory(dir)
                }
            },
        )
        contextMenu(
            action("Fetch / Update", AllIcons.Actions.Download) {
                selectedItem()?.data?.let { service.fetchRepository(it.str("alias"), null) }
            },
            action("Reveal Folder", AllIcons.Actions.MenuOpen) {
                selectedItem()?.data?.get("location")?.obj()?.str("dir")?.takeIf { it.isNotEmpty() }?.let {
                    RevealFileAction.openDirectory(File(it))
                }
            },
        )
        reload()
    }

    override fun load() = service.repositories()

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        val repos = json.arr().mapNotNull { it.obj() }
        if (repos.isEmpty()) {
            root.add(DefaultMutableTreeNode(Item("No resources.repositories in this project")))
            return
        }
        for (r in repos) {
            val loc = r.get("location").obj() ?: JsonObject()
            val state = loc.str("state")
            val icon = when (state) {
                "ready" -> AllIcons.Nodes.Module
                "fetching" -> AllIcons.Process.Step_1
                "missing" -> AllIcons.Actions.Download
                else -> AllIcons.General.Warning
            }
            val tip = buildString {
                append("<html><b>${r.str("name")}</b> (@${r.str("alias")})")
                loc.str("url").takeIf { it.isNotEmpty() }?.let { append("<br>URL: $it") }
                loc.str("dir").takeIf { it.isNotEmpty() }?.let { append("<br>Folder: $it") }
                loc.str("commit").takeIf { it.isNotEmpty() }?.let { append("<br>Commit: ${it.take(12)}") }
                loc.str("error").takeIf { it.isNotEmpty() }?.let { append("<br>⚠ $it") }
                append("</html>")
            }
            val node = DefaultMutableTreeNode(Item(r.str("label"), r.str("detail"), icon, tip,
                error = state == "failed" || state == "unconfigured", data = r,
                open = { if (state == "ready") loc.str("dir").takeIf { it.isNotEmpty() }?.let { RevealFileAction.openDirectory(File(it)) } else service.fetchRepository(r.str("alias"), null) }))
            r.get("declaredIn").arr().forEach { node.add(DefaultMutableTreeNode(Item(it.asString, "declared in", AllIcons.FileTypes.Yaml))) }
            root.add(node)
        }
    }
}


// ── Stats ─────────────────────────────────────────────────────────────────
class StatsPanel(project: Project) : ServerTreePanel(project) {
    init {
        toolbar(
            action("Refresh", AllIcons.Actions.Refresh) { service.execute("atn.refresh"); reload() },
            action("Expand All", AllIcons.Actions.Expandall) { TreeUtil.expandAll(tree) },
            action("Collapse All", AllIcons.Actions.Collapseall) { TreeUtil.collapseAll(tree, 1) },
        )
        contextMenu(
            action("Open", AllIcons.Actions.EditSource) { selectedItem()?.data?.str("filePath")?.takeIf { it.isNotEmpty() }?.let { service.openFile(it) } },
            action("Show in Graph", AllIcons.Graph.Layout) { selectedItem()?.data?.let { showInGraph(it) } },
        )
        reload()
    }

    override fun load() = service.stats()

    private fun showInGraph(row: JsonObject) {
        when (row.str("focus")) {
            "file" -> GraphPanel.show(project) { it.focusFile(row.str("filePath")) }
            "repo" -> GraphPanel.show(project) { it.showFiltered(repo = row.str("repo")) }
            "problems" -> GraphPanel.show(project) { it.showFiltered(problems = true) }
        }
    }

    private fun icon(r: JsonObject): Icon = when (r.str("kind")) {
        "error", "missing" -> AllIcons.General.Error
        "warning" -> AllIcons.General.Warning
        "info" -> AllIcons.General.Information
        "pipeline" -> AllIcons.Actions.Execute
        "external", "repo" -> if (r.int("severity") == 1) AllIcons.General.Error else AllIcons.Nodes.Module
        else -> if (r.int("severity") == 1) AllIcons.General.Error else if (r.int("severity") == 2) AllIcons.General.Warning else AllIcons.FileTypes.Yaml
    }

    /** A row, with its children (the files under a problem type) nested below it. */
    private fun rowNode(r: JsonObject): DefaultMutableTreeNode {
        val open: (() -> Unit)? = when {
            r.str("filePath").isNotEmpty() -> { { service.openFile(r.str("filePath"), r.int("line")) } }
            r.str("focus").isNotEmpty() -> { { showInGraph(r) } }
            else -> null
        }
        val node = DefaultMutableTreeNode(Item(r.str("label"), r.str("description"), icon(r),
            r.str("tooltip").ifEmpty { null }?.let { "<html>" + StringUtil.escapeXmlEntities(it).replace("\n", "<br>") + "</html>" },
            data = r, open = open))
        for (c in r.list("children")) node.add(rowNode(c))
        return node
    }

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        val sections = json.arr().mapNotNull { it.obj() }
        if (sections.isEmpty()) {
            root.add(DefaultMutableTreeNode(Item("No statistics yet")))
            return
        }
        for (s in sections) {
            val node = DefaultMutableTreeNode(Item(s.str("label"), s.str("description"), null, s.str("tooltip").ifEmpty { null }))
            for (r in s.list("rows")) node.add(rowNode(r))
            root.add(node)
        }
    }
}
