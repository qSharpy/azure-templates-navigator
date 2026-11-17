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
import com.bogdanbujor.azuretemplates.settings.AtnSettings
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
import com.intellij.openapi.wm.ToolWindowManager
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
import javax.swing.JList
import com.intellij.ui.ColoredListCellRenderer
import com.intellij.openapi.ui.popup.JBPopupFactory
import com.intellij.openapi.ui.Messages
import com.intellij.openapi.project.DumbAware
import com.intellij.openapi.actionSystem.ToggleAction
import com.intellij.ide.util.PropertiesComponent
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
        add("Pipelines") { PipelinesPanel(project) }
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
/** Shows the Azure Templates tool window on its first tab of [type]. */
fun <T : JComponent> showTab(project: Project, type: Class<T>, then: (T) -> Unit = {}) {
    val tw = ToolWindowManager.getInstance(project).getToolWindow("Azure Templates") ?: return
    tw.show {
        val tabs = tw.contentManager.contents.firstNotNullOfOrNull { it.component as? AtnTabs } ?: return@show
        then(tabs.select(type) ?: return@show)
    }
}

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
    /** Expanded when the tree is built. */
    val expand: Boolean = false,
)

internal const val MY_CHANGES_KEY = "azureTemplatesNavigator.pipelines.onlyMyChanges"

/** Base class: a tree fed by an async server request, refreshed on change. */
abstract class ServerTreePanel(protected val project: Project) : SimpleToolWindowPanel(true, true), Disposable {
    protected val service = AtnService.getInstance(project)
    protected val tree = Tree(DefaultTreeModel(DefaultMutableTreeNode()))

    /** Expand every top-level node on reload (otherwise only those the user had open). */
    protected open val expandTopLevel = true

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

    private val props = PropertiesComponent.getInstance(project)

    /** "Only My Changes", shared by the Pipelines and Diagnostics tabs and remembered per project. */
    protected var onlyMyChanges: Boolean
        get() = props.getBoolean(MY_CHANGES_KEY, false)
        set(v) = props.setValue(MY_CHANGES_KEY, v, false)

    /** The toolbar's Only My Changes toggle and Compare My Changes With… (Pipelines and Diagnostics tabs). */
    protected fun myChangesActions(): Array<AnAction> = arrayOf(
        object : ToggleAction("Only My Changes", "Show only what your changes did since your work started: the problems they added and fixed, and the pipelines they reach", AllIcons.Actions.Diff), DumbAware {
            override fun getActionUpdateThread() = ActionUpdateThread.EDT
            override fun isSelected(e: AnActionEvent) = onlyMyChanges
            override fun setSelected(e: AnActionEvent, state: Boolean) { onlyMyChanges = state; service.fireChanged() }
        },
        action("Compare My Changes With…", AllIcons.Vcs.Branch) { chooseBase() },
    )

    /** Pick what "my changes" are compared with; the choice is stored per branch (git config). */
    protected fun chooseBase() {
        service.changeBases().thenAccept { data ->
            val choices = data.arr().mapNotNull { it.obj() }.toMutableList()
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed) return@invokeLater
                if (choices.isEmpty()) {
                    Messages.showInfoMessage(project, "Changes are compared with a git commit: open a project that is a git repository.", "Compare My Changes")
                    return@invokeLater
                }
                val other = JsonObject().apply { addProperty("label", "Another branch, tag or commit…"); addProperty("other", true) }
                choices += other
                JBPopupFactory.getInstance().createPopupChooserBuilder(choices)
                    .setTitle("Compare My Changes With")
                    .setNamerForFiltering { it.str("label") }
                    .setRenderer(object : ColoredListCellRenderer<JsonObject>() {
                        override fun customizeCellRenderer(list: JList<out JsonObject>, value: JsonObject, index: Int, selected: Boolean, hasFocus: Boolean) {
                            icon = when {
                                value.has("other") -> AllIcons.Actions.Edit
                                value.get("picked")?.asBoolean == true -> AllIcons.Actions.Checked
                                value.str("ref") == "HEAD" -> AllIcons.Vcs.CommitNode
                                value.str("ref").isEmpty() -> AllIcons.Actions.Lightning
                                else -> AllIcons.Vcs.Branch
                            }
                            append(value.str("label"))
                            value.str("detail").takeIf { it.isNotEmpty() }?.let { append("  $it", SimpleTextAttributes.GRAYED_ATTRIBUTES) }
                        }
                    })
                    .setItemChosenCallback { pick ->
                        val ref = if (pick.has("other"))
                            Messages.showInputDialog(project, "A branch, tag or commit to compare with", "Compare My Changes", null, "", null) ?: return@setItemChosenCallback
                        else pick.str("ref")
                        setBase(ref)
                    }
                    .createPopup()
                    .showInCenterOf(this)
            }
        }
    }

    private fun setBase(ref: String) {
        service.setChangesBase(ref).whenComplete { _, err ->
            ApplicationManager.getApplication().invokeLater {
                if (project.isDisposed) return@invokeLater
                if (err != null) {
                    val msg = generateSequence(err) { it.cause }.last().message ?: err.toString()
                    Messages.showErrorDialog(project, "Can't compare with $ref: $msg", "Compare My Changes")
                    return@invokeLater
                }
                onlyMyChanges = true
                service.fireChanged() // both tabs reload

            }
        }
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
                    if ((expandTopLevel && node.level == 1) || (item != null && (item.expand || item.label in expanded))) {
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
        toolbar(*myChangesActions(), action("Refresh", AllIcons.Actions.Refresh) { service.execute("atn.refresh"); reload() })
        reload()
    }

    override fun load() = service.workspaceDiagnostics(onlyNew = onlyMyChanges)

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        if (onlyMyChanges) {
            // Only My Changes: {available, reason, label, files, newErrors, newWarnings, existing}
            val c = json.obj()
            if (c == null) {
                root.add(DefaultMutableTreeNode(Item("Comparing with where your work started…", "waiting for the language server", AllIcons.Process.Step_1)))
                return
            }
            if (c.get("available")?.asBoolean != true) {
                root.add(DefaultMutableTreeNode(Item(c.str("reason").ifEmpty { "Nothing to compare with" }, icon = AllIcons.General.Information,
                    tooltip = "Changes are compared with a git commit: open a project that is a git repository")))
                return
            }
            val existing = c.int("existing")
            root.add(DefaultMutableTreeNode(Item("Since ${c.str("label")}", if (existing > 0) "$existing already there, hidden" else "", AllIcons.Vcs.Branch,
                "<html>Only the problems your changes added. $existing that were already there are hidden: turn off Only My Changes to see them.<br><br>" +
                    "Double-click to compare with another branch or commit</html>", open = { chooseBase() })))
            val files = c.list("files")
            if (files.isEmpty()) root.add(DefaultMutableTreeNode(Item("No new problems", icon = AllIcons.General.InspectionsOK)))
            files.forEach { root.add(fileNode(it, expand = true)) }
            return
        }
        val files = json.arr().mapNotNull { it.obj() }
        if (files.isEmpty()) {
            root.add(DefaultMutableTreeNode(Item("No problems found", icon = AllIcons.General.InspectionsOK)))
            return
        }
        files.forEach { root.add(fileNode(it)) }
    }

    private fun fileNode(f: JsonObject, expand: Boolean = false): DefaultMutableTreeNode {
        val counts = listOfNotNull(
            f.int("errors").takeIf { it > 0 }?.let { "$it error(s)" },
            f.int("warnings").takeIf { it > 0 }?.let { "$it warning(s)" },
        ).joinToString(", ")
        val path = f.str("filePath")
        val fileNode = DefaultMutableTreeNode(Item(f.str("relativePath"), counts, AllIcons.FileTypes.Yaml, open = { service.openFile(path) }, expand = expand))
        for (d in f.list("diagnostics")) {
            val err = d.int("severity") == 1
            fileNode.add(DefaultMutableTreeNode(Item(d.str("message"), "Ln ${d.int("line") + 1}",
                if (err) AllIcons.General.Error else AllIcons.General.Warning, "[${d.str("code")}]",
                open = { service.openFile(path, d.int("line")) })))
        }
        return fileNode
    }
}

// ── Pipelines ─────────────────────────────────────────────────────────────
/**
 * Two views, switched with "Only My Changes" in the toolbar (remembered per project):
 *  - every root pipeline with its verdict ("Will fail when queued: 2 errors"), failing
 *    first, from atn/pipelineCheck without a uri; problems your changes added say "new";
 *  - what your changes did since the commit your work started from (atn/changes): the
 *    base on top (double-click to compare with something else), the files you changed,
 *    the totals, then each pipeline your changes reach with its new and fixed problems.
 * Double-click a pipeline or problem to open it.
 */
class PipelinesPanel(project: Project) : ServerTreePanel(project) {
    override val expandTopLevel = false

    init {
        toolbar(
            *myChangesActions(),
            action("Check in Azure DevOps", AllIcons.Actions.Execute) { AzureCheck.run(project) },
            action("Refresh", AllIcons.Actions.Refresh) { service.execute("atn.refresh"); reload() },
            action("Expand All", AllIcons.Actions.Expandall) { TreeUtil.expandAll(tree) },
            action("Collapse All", AllIcons.Actions.Collapseall) { TreeUtil.collapseAll(tree, 1) },
        )
        contextMenu(action("Open", AllIcons.Actions.EditSource) { selectedItem()?.open?.invoke() })
        reload()
    }

    override fun load() = if (onlyMyChanges) service.changes() else service.pipelineChecks()

    private fun verdictIcon(verdict: String): Icon = when (verdict) {
        "fail" -> AllIcons.General.Error
        "warn" -> AllIcons.General.Warning
        "ok" -> AllIcons.General.InspectionsOK
        else -> AllIcons.General.Remove
    }

    override fun build(root: DefaultMutableTreeNode, json: JsonElement?) {
        if (onlyMyChanges) buildChanges(root, json.obj()) else buildAll(root, json)
    }

    private fun problemNode(p: JsonObject, fixed: Boolean = false): DefaultMutableTreeNode {
        val line = p.get("range").obj()?.get("start").obj()?.int("line") ?: 0
        val where = "${p.str("relativePath")}:${line + 1}"
        val isNew = p.get("new")?.takeIf { it.isJsonPrimitive }?.asBoolean == true
        val icon = when {
            fixed -> AllIcons.General.InspectionsOK
            p.int("severity") == 1 -> AllIcons.General.Error
            else -> AllIcons.General.Warning
        }
        val note = if (fixed) "<br>Fixed: it was there at the base and is gone now" else if (isNew) "<br>New: your changes added it" else ""
        return DefaultMutableTreeNode(Item(p.str("message"), (if (isNew) "new · " else "") + where, icon,
            "<html>${StringUtil.escapeXmlEntities(where)} [${StringUtil.escapeXmlEntities(p.str("code"))}]$note</html>",
            data = p, open = { service.openFile(p.str("filePath"), line) }))
    }

    private fun buildAll(root: DefaultMutableTreeNode, json: JsonElement?) {
        if (json == null || json.isJsonNull) {
            // null: diagnostics are off (no verdicts), or the server isn't up yet
            val off = !AtnSettings.getInstance().state.diagnosticsEnabled
            root.add(DefaultMutableTreeNode(
                if (off) Item("Diagnostics are turned off", "Settings → Tools → Azure Templates Navigator", AllIcons.General.Information)
                else Item("Checking pipelines…", "waiting for the language server", AllIcons.Process.Step_1)))
            return
        }
        val reports = json.arr().mapNotNull { it.obj() }
        if (reports.isEmpty()) {
            root.add(DefaultMutableTreeNode(Item("No pipelines found in this project", icon = AllIcons.General.Information,
                tooltip = "A pipeline has pipeline-level keys (trigger, pr, schedules, resources, extends, pool) and no other file uses it as a template")))
            return
        }
        val failing = reports.count { it.str("verdict") == "fail" }
        val warned = reports.count { it.str("verdict") == "warn" }
        val fresh = reports.sumOf { it.get("change").obj()?.int("new") ?: 0 }
        val summary = when {
            failing > 0 -> "$failing of ${reports.size} pipelines will fail when queued"
            warned > 0 -> "$warned of ${reports.size} pipelines may fail while running"
            else -> "All ${reports.size} pipelines: no problems found"
        }
        root.add(DefaultMutableTreeNode(Item(summary, if (fresh > 0) "+$fresh new from your changes" else "",
            verdictIcon(if (failing > 0) "fail" else if (warned > 0) "warn" else "ok"))))
        for (r in reports) {
            val path = r.str("filePath")
            val change = r.get("change").obj()
            val desc = r.str("title").substringAfter(' ') + (change?.int("new")?.takeIf { it > 0 }?.let { " · +$it new" } ?: "")
            val tip = "<html>${StringUtil.escapeXmlEntities(r.str("title"))}<br>${r.int("templates")} template(s) checked" +
                (change?.let { "<br>Your changes: ${StringUtil.escapeXmlEntities(it.str("title"))}" } ?: "") + "</html>"
            val node = DefaultMutableTreeNode(Item(r.str("relativePath"), desc, verdictIcon(r.str("verdict")), tip,
                data = r, open = { service.openFile(path) }))
            for (p in r.list("problems")) node.add(problemNode(p))
            root.add(node)
        }
    }

    private fun ago(iso: String): String {
        val t = try { java.time.Instant.parse(iso).toEpochMilli() } catch (e: Exception) { return "" }
        val m = (System.currentTimeMillis() - t) / 60000
        return if (m < 1) "just now" else if (m < 60) "$m min ago" else "${(m + 30) / 60} h ago"
    }

    /** A pipeline's last check in Azure DevOps (atn/changes: pipelines[].azure). */
    private fun azureNode(a: JsonObject): DefaultMutableTreeNode {
        val agreement = when (a.str("agreement")) {
            "agree" -> "atn agrees"
            "azureOnly" -> "atn found no error here: please report it"
            "atnOnly" -> "atn reports errors Azure accepts"
            else -> ""
        }
        val stale = a.get("stale")?.asBoolean == true
        val icon = when {
            a.str("skip").isNotEmpty() -> AllIcons.General.Remove
            a.str("verdict") == "compiles" -> AllIcons.General.InspectionsOK
            a.str("verdict") == "rejected" -> AllIcons.General.Error
            else -> AllIcons.General.QuestionDialog
        }
        val def = a.get("definition").obj()
        val params = a.get("parameters").obj()
        val tip = listOfNotNull(
            a.str("title"),
            def?.let { "Compiled with pipeline #${it.int("id")} ${it.str("name")}" },
            a.str("before").takeIf { it.isNotEmpty() }?.let { "At the base: $it" },
            params?.let { o -> "Runtime parameters filled in: " + o.entrySet().joinToString(", ") { "${it.key}=${it.value}" } },
            a.str("error").takeIf { it.isNotEmpty() },
        ).joinToString("<br>") { StringUtil.escapeXmlEntities(it) }
        val title = a.str("title").replace(Regex(" · atn .*$"), "").replace(Regex(" · expansion .*$"), "")
        val node = DefaultMutableTreeNode(Item("Azure DevOps: $title", (if (stale) "out of date · " else "") + agreement, icon,
            "<html>$tip</html>", data = a, expand = true))
        fun asProblem(x: JsonObject, isNew: Boolean) = x.deepCopy().apply {
            addProperty("severity", 1); addProperty("code", "azure"); addProperty("new", isNew)
        }
        val news = a.list("new")
        news.forEach { node.add(problemNode(asProblem(it, true))) }
        fun line(x: JsonObject) = x.get("range").obj()?.get("start").obj()?.int("line") ?: 0
        a.list("problems").filter { p -> news.none { it.str("message") == p.str("message") && it.str("filePath") == p.str("filePath") && line(it) == line(p) } }
            .forEach { node.add(problemNode(asProblem(it, false))) }
        val ex = a.get("expansion").obj()
        if (ex?.get("changed")?.asBoolean == true) {
            val path = a.str("filePath")
            node.add(DefaultMutableTreeNode(Item("Expanded YAML changed (+${ex.int("added")} −${ex.int("removed")})", "double-click for the diff",
                AllIcons.Actions.Diff, "The pipeline as Azure DevOps expanded it, at the base and with your changes",
                open = { AzureCheck.showDiff(project, path) })))
        }
        if (a.str("agreement") == "atnOnly") {
            node.add(DefaultMutableTreeNode(Item("Azure compiles it, but atn reports errors", icon = AllIcons.General.Information,
                tooltip = "Azure's compile check passed. atn's errors may still fail the run when it is queued, or atn is wrong: please report it if so.")))
        }
        return node
    }

    private fun buildChanges(root: DefaultMutableTreeNode, c: JsonObject?) {
        if (c == null) {
            root.add(DefaultMutableTreeNode(Item("Comparing with where your work started…", "waiting for the language server", AllIcons.Process.Step_1)))
            return
        }
        if (c.get("available")?.asBoolean != true) {
            root.add(DefaultMutableTreeNode(Item(c.str("reason").ifEmpty { "Nothing to compare with" }, icon = AllIcons.General.Information,
                tooltip = "Changes are compared with a git commit: open a project that is a git repository")))
            return
        }
        val bases = c.list("bases")
        val base = bases.firstOrNull()
        val commits = base?.int("commits") ?: 0
        val baseDesc = when {
            commits > 0 -> "$commits commit${if (commits == 1) "" else "s"} + uncommitted"
            base?.str("ref") == "HEAD" -> ""
            else -> "uncommitted"
        }
        val baseTip = bases.joinToString("<br>") { b ->
            StringUtil.escapeXmlEntities("${b.str("repo")}: ${b.str("detail")}") + (b.str("error").takeIf { it.isNotEmpty() }?.let { "<br>⚠ " + StringUtil.escapeXmlEntities(it) } ?: "")
        }
        root.add(DefaultMutableTreeNode(Item("Since ${c.str("label")}", baseDesc,
            if (bases.any { it.str("error").isNotEmpty() }) AllIcons.General.Warning else AllIcons.Vcs.Branch,
            "<html>$baseTip<br><br>Double-click to compare with another branch or commit</html>", open = { chooseBase() })))
        // what you changed first, then what it did to the pipelines
        val files = c.list("files")
        if (files.isNotEmpty()) {
            val group = DefaultMutableTreeNode(Item("Files changed (${files.size})", icon = AllIcons.Actions.ListChanges, expand = true))
            for (f in files) {
                val parts = mutableListOf(f.str("status"))
                f.int("pipelines").takeIf { it > 0 }?.let { parts += "$it pipeline${if (it == 1) "" else "s"} use it" }
                f.list("new").size.takeIf { it > 0 }?.let { parts += "+$it new" }
                f.list("fixed").size.takeIf { it > 0 }?.let { parts += "$it fixed" }
                val path = f.str("filePath")
                val node = DefaultMutableTreeNode(Item(f.str("relativePath"), parts.joinToString(" · "), AllIcons.FileTypes.Yaml,
                    data = f, open = if (f.str("status") != "deleted") ({ service.openFile(path) }) else null))
                f.list("new").forEach { node.add(problemNode(it)) }
                f.list("fixed").forEach { node.add(problemNode(it, fixed = true)) }
                group.add(node)
            }
            root.add(group)
        }
        val newErrors = c.int("newErrors")
        root.add(DefaultMutableTreeNode(Item(c.str("summary"), "${c.int("allFailing")} of ${c.int("allPipelines")} fail in total",
            if (newErrors > 0) AllIcons.General.Error else AllIcons.General.InspectionsOK,
            "<html>What your changes did to the pipelines below<br><br>In total, ${c.int("allFailing")} of ${c.int("allPipelines")} pipelines fail when queued, " +
                "counting problems that were there before your changes.<br>Double-click to see every pipeline</html>", open = { onlyMyChanges = false; reload() })))
        // Check in Azure DevOps: the last check, or an offer to run one
        val az = c.get("azure").obj()
        if (az != null || c.get("azureEnabled")?.asBoolean == true) {
            val running = az?.str("state") == "running"
            val stale = az?.get("stale")?.asBoolean == true
            val totals = az?.get("totals").obj()
            val warn = az != null && (az.str("state") == "failed" || stale || (totals?.int("withNewProblems") ?: 0) > 0 || (totals?.int("disagree") ?: 0) > 0)
            val desc = when {
                az == null -> "compile these pipelines in Azure DevOps"
                running -> ""
                stale -> "out of date · double-click to check again"
                else -> ago(az.str("finishedAt"))
            }
            val error = az?.str("error")?.takeIf { it.isNotEmpty() }?.let { StringUtil.escapeXmlEntities(it) + "<br><br>" } ?: ""
            root.add(DefaultMutableTreeNode(Item(az?.str("summary") ?: "Check in Azure DevOps", desc,
                when { running -> AllIcons.Process.Step_1; warn -> AllIcons.General.Warning; else -> AllIcons.Actions.Execute },
                "<html>${error}Azure DevOps compiles each pipeline below, at the base and with your changes, without running it. Double-click to check again.<br><br>" +
                    "Azure checks what makes a run fail when it is queued (YAML, templates, parameters, dependsOn, tasks, environments). " +
                    "It doesn't check pools, variable groups, service connections or task inputs, and PR-only branches (Build.Reason) can't be previewed.</html>",
                open = { AzureCheck.run(project) })))
        }
        for (p in c.list("pipelines")) {
            val path = p.str("filePath")
            val now = p.get("now").obj()
            val before = p.get("before").obj()
            fun state(v: JsonObject?, gone: String) =
                if (v?.get("exists")?.asBoolean == true) "${v.int("errors")} error(s), ${v.int("warnings")} warning(s)" else gone
            val exists = now?.get("exists")?.asBoolean == true
            val node = DefaultMutableTreeNode(Item(p.str("relativePath"), p.str("title"), verdictIcon(now?.str("verdict") ?: ""),
                "<html>Before: ${state(before, "did not exist")}<br>Now: ${state(now, "removed")}</html>",
                data = p, open = if (exists) ({ service.openFile(path) }) else null))
            val news = p.list("new")
            val fixed = p.list("fixed")
            val changed = p.get("changed").arr().map { it.asString }
            p.get("azure").obj()?.let { node.add(azureNode(it)) }
            if (news.isNotEmpty()) node.add(DefaultMutableTreeNode(Item("New (${news.size})", icon = AllIcons.General.Add)).apply { news.forEach { add(problemNode(it)) } })
            if (fixed.isNotEmpty()) node.add(DefaultMutableTreeNode(Item("Fixed (${fixed.size})", icon = AllIcons.General.InspectionsOK)).apply { fixed.forEach { add(problemNode(it, fixed = true)) } })
            if (changed.isNotEmpty()) node.add(DefaultMutableTreeNode(Item("Files you changed that it uses (${changed.size})", icon = AllIcons.Actions.Diff)).apply {
                changed.forEach { add(DefaultMutableTreeNode(Item(it, icon = AllIcons.FileTypes.Yaml))) }
            })
            root.add(node)
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
                selectedItem()?.data?.let { r ->
                    // the first declaring file, where the server resolves the alias
                    service.fetchRepository(r.str("alias"), r.list("declarations").firstOrNull()?.str("filePath")?.ifEmpty { null })
                }
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
            val templates = r.list("templates")
            val decls = r.list("declarations")
            val firstDecl = decls.firstOrNull()?.str("filePath")?.ifEmpty { null }
            val used = "${templates.size} template${if (templates.size == 1) "" else "s"} used"
            val node = DefaultMutableTreeNode(Item(r.str("label"), "${r.str("detail")} · $used", icon, tip,
                error = state == "failed" || state == "unconfigured", data = r,
                open = { if (state == "ready") loc.str("dir").takeIf { it.isNotEmpty() }?.let { RevealFileAction.openDirectory(File(it)) } else service.fetchRepository(r.str("alias"), firstDecl) }))
            // What the project uses from it, most used first; a broken one opens its first call.
            for (t in templates) {
                val n = t.int("usages")
                val files = t.int("callers")
                val broken = t.get("broken")?.asBoolean == true
                val uses = "$n usage${if (n == 1) "" else "s"} · $files file${if (files == 1) "" else "s"}"
                val ttip = "<html>" + StringUtil.escapeXmlEntities("${t.str("label")}@${r.str("alias")}") +
                    (if (broken) "<br>Not found. Opens the first call." else "<br>Used $uses in this project") + "</html>"
                node.add(DefaultMutableTreeNode(Item(t.str("label"), if (broken) "$uses · not found" else uses,
                    if (broken) AllIcons.General.Error else AllIcons.FileTypes.Yaml, ttip, error = broken,
                    open = { if (broken) service.openFile(t.str("refFile"), t.int("refLine")) else service.openFile(t.str("filePath")) })))
            }
            if (decls.isNotEmpty()) {
                val group = DefaultMutableTreeNode(Item("Declared in", "${decls.size} file${if (decls.size == 1) "" else "s"}", AllIcons.Nodes.Folder,
                    "Files whose resources.repositories declares this repository"))
                for (d in decls) {
                    group.add(DefaultMutableTreeNode(Item(d.str("relativePath"), "line ${d.int("line") + 1}", AllIcons.FileTypes.Yaml,
                        open = { service.openFile(d.str("filePath"), d.int("line")) })))
                }
                node.add(group)
            }
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
            "kind" -> GraphPanel.show(project) { g -> g.showFiltered(kinds = row.get("kinds").arr().map { it.asString }) }
        }
    }

    private fun openView(view: String) {
        when (view) {
            "pipelines" -> showTab(project, PipelinesPanel::class.java)
            "repositories" -> showTab(project, RepositoriesPanel::class.java)
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
        // A row opens its file; a row with children only expands (its graph is in
        // the context menu); else the tab or graph it points at.
        val open: (() -> Unit)? = when {
            r.str("filePath").isNotEmpty() -> { { service.openFile(r.str("filePath"), r.int("line")) } }
            r.list("children").isNotEmpty() -> null
            r.str("view").isNotEmpty() -> { { openView(r.str("view")) } }
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
