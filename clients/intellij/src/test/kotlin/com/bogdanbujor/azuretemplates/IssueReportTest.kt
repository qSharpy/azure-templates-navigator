package com.bogdanbujor.azuretemplates

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.net.URI
import java.net.URLDecoder
import java.nio.charset.StandardCharsets

class IssueReportTest {
    private val env = listOf("Azure Templates Navigator 2.1.0", "atn 2.1.0", "IntelliJ IDEA 2024.2.4", "Mac OS X 15.1 aarch64")

    private fun params(url: String): Map<String, String> =
        URI(url).rawQuery.split("&").associate {
            val (k, v) = it.split("=", limit = 2)
            k to URLDecoder.decode(v, StandardCharsets.UTF_8)
        }

    @Test
    fun bugReportHasSectionsAndEnvironment() {
        val url = IssueReport.url(IssueReport.Kind.BUG, env)
        assertTrue(url.startsWith("${IssueReport.ISSUES_URL}/new?"))
        val p = params(url)
        assertEquals("bug", p["labels"])
        val body = p.getValue("body")
        assertTrue(body.contains("### What happened?"))
        assertTrue(body.contains("### Environment\n\n- Azure Templates Navigator 2.1.0\n- atn 2.1.0\n"))
    }

    @Test
    fun featureRequestUsesEnhancementLabel() {
        val p = params(IssueReport.url(IssueReport.Kind.FEATURE, env))
        assertEquals("enhancement", p["labels"])
        assertTrue(p.getValue("body").contains("### What would you like?"))
    }

    @Test
    fun specialCharactersSurviveEncoding() {
        val logs = "a & b = c # d ? e + f\nline 2 ünïcode"
        val p = params(IssueReport.url(IssueReport.Kind.BUG, env, title = "Error: x & y", logs = logs))
        assertEquals("Error: x & y", p["title"])
        assertTrue(p.getValue("body").contains("```text\n$logs\n```"))
    }

    @Test
    fun longStackTracesAreTruncatedToFitTheUrl() {
        val trace = "java.lang.IllegalStateException: boom\n" + (1..2000).joinToString("\n") { "\tat com.example.Frame$it.call(Frame.kt:$it)" }
        val url = IssueReport.url(IssueReport.Kind.BUG, env, title = "Error: boom", logs = trace)
        assertTrue("url is ${url.length} chars", url.length <= IssueReport.MAX_URL)
        val body = params(url).getValue("body")
        assertTrue(body.contains("java.lang.IllegalStateException: boom"))
        assertTrue(body.contains("… (truncated)"))
        assertTrue(body.contains("### Environment"))
    }
}
