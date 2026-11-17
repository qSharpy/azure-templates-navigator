package com.bogdanbujor.azuretemplates

import com.google.gson.JsonParser
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Every tip in shared/tips/tips.json (also the VS Code walkthrough) is listed in plugin.xml, in
 * the same order, and Gradle generated its HTML in the IDE's Tip of the Day format.
 */
class TipsParityTest {
    private val tips = JsonParser.parseString(File("../../shared/tips/tips.json").readText()).asJsonObject
        .getAsJsonArray("tips").map { it.asJsonObject }

    @Test
    fun pluginXmlListsEveryTip() {
        val xml = File("src/main/resources/META-INF/plugin.xml").readText()
        val listed = Regex("""<tipAndTrick file="([^"]+)"/>""").findAll(xml).map { it.groupValues[1] }.toList()
        assertEquals(tips.map { "AzureTemplates_${it["id"].asString}.html" }, listed)
    }

    @Test
    fun generatedTipsAreTipOfTheDayHtml() {
        for (t in tips) {
            val id = t["id"].asString
            val html = javaClass.getResource("/tips/AzureTemplates_$id.html")?.readText()
            assertNotNull("tips/AzureTemplates_$id.html", html)
            assertTrue(id, html!!.contains("""href="css/tips.css""""))
            assertTrue(id, html.contains("<p><b>${t["title"].asString}</b></p>"))
            assertTrue(id, !html.contains("{shortcut:") && !html.contains("**"))
        }
    }
}
