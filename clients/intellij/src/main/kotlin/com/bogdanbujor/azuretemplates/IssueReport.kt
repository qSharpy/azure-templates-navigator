package com.bogdanbujor.azuretemplates

import java.net.URLEncoder
import java.nio.charset.StandardCharsets

/**
 * Builds the "new issue" URL that Report Issue and the IDE error reporter open.
 * Same format as the VS Code client (clients/vscode/src/issue.js): title and
 * body query parameters, which work without the issue forms and without a
 * running server. Only versions and the OS go in the body, plus the stack
 * trace for errors; the user reviews it before submitting.
 */
object IssueReport {
    const val ISSUES_URL = "https://github.com/qSharpy/azure-templates-navigator/issues"
    /** The feature guide: every feature with screenshots and shortcuts, for both IDEs. */
    const val DOCS_URL = "https://github.com/qSharpy/azure-templates-navigator/blob/main/docs/README.md"
    /** "What's new": the changelog shared by the server and both clients. */
    const val CHANGELOG_URL = "https://github.com/qSharpy/azure-templates-navigator/blob/main/CHANGELOG.md"

    /** "Give Feedback": the plugin's reviews on JetBrains Marketplace (30272 is its Marketplace id). */
    const val REVIEW_URL = "https://plugins.jetbrains.com/plugin/30272-azure-templates-navigator/reviews"

    /** GitHub rejects very long URLs; stay well below the limit. */
    const val MAX_URL = 7000

    enum class Kind(val label: String) { BUG("bug"), FEATURE("enhancement") }

    fun url(kind: Kind, environment: List<String>, title: String = "", logs: String = ""): String {
        var log = logs.trim()
        while (true) {
            val url = build(kind, environment, title, log)
            if (url.length <= MAX_URL) return url
            if (log.isEmpty()) return "$ISSUES_URL/new?" + query("labels" to kind.label, "title" to title)
            // Keep the top of the trace: the exception and the frames that threw it.
            log = if (log.length < 200) "" else log.take(log.length * 3 / 4).trimEnd() + "\n… (truncated)"
        }
    }

    private fun build(kind: Kind, environment: List<String>, title: String, logs: String): String {
        val sections = when (kind) {
            Kind.BUG -> listOf(
                "What happened?" to "<!-- What you did, what you saw, and what you expected instead. -->",
                "YAML that shows the problem" to "<!-- The smallest pipeline and template that reproduce it. Remove anything private. -->\n\n```yaml\n\n```",
                "Log output" to (if (logs.isEmpty()) "<!-- Help | Show Log, lines starting with [atn] -->\n\n```text\n\n```"
                                 else "<!-- Check it for anything private before submitting. -->\n\n```text\n$logs\n```"),
            )
            Kind.FEATURE -> listOf(
                "What would you like?" to "<!-- What you are trying to do, and what would make it easier. Feedback is welcome too. -->",
            )
        }
        val body = sections.joinToString("\n") { (h, text) -> "### $h\n\n$text\n" } +
            "\n### Environment\n\n" + environment.joinToString("\n") { "- $it" } + "\n"
        return "$ISSUES_URL/new?" + query("labels" to kind.label, "title" to title, "body" to body)
    }

    private fun query(vararg params: Pair<String, String>): String =
        params.joinToString("&") { (k, v) -> "$k=" + URLEncoder.encode(v, StandardCharsets.UTF_8) }
}
