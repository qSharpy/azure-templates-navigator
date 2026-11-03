package com.bogdanbujor.azuretemplates.ui

// Tool window tabs. Each tab renders view models computed by the server
// (atn/dependencyTree, atn/workspaceDiagnostics, atn/repositories), so the
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
import com.intellij.ui.SimpleTextAttributes
import com.intellij.ui.content.ContentFactory
import com.intellij.ui.treeStructure.Tree
import com.intellij.util.ui.tree.TreeUtil
import java.awt.event.KeyAdapter
import java.awt.event.KeyEvent
import java.awt.event.MouseAdapter
import java.awt.event.MouseEvent
import java.io.File
import java.util.concurrent.CompletableFuture
import javax.swing.Icon
import javax.swing.JTree
import javax.swing.tree.DefaultMutableTreeNode
import javax.swing.tree.DefaultTreeModel

class AtnToolWindowFactory : ToolWindowFactory {
    override fun createToolWindowContent(project: Project, toolWindow: ToolWindow) {
        val cf = ContentFactory.getInstance()
        fun add(title: String, panel: SimpleToolWindowPanel) {
            val content = cf.createContent(panel, title, false)
            if (panel is Disposable) content.setDisposer(panel)
            toolWindow.contentManager.addContent(content)
        }
        add("Dependencies", DependenciesPanel(project))
        add("Graph", GraphPanel(project))
        add("Diagnostics", DiagnosticsPanel(project))
        add("Repositories", RepositoriesPanel(project))
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

