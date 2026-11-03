package com.bogdanbujor.azuretemplates

import com.bogdanbujor.azuretemplates.settings.AtnSettings
import com.google.gson.JsonObject
import com.google.gson.JsonParser
import org.junit.Assert.assertEquals
import org.junit.Test
import java.io.File

/**
 * Guards against configuration drift: the settings JSON this plugin sends to
 * the server must have exactly the same keys as the VS Code extension's
 * settings (clients/vscode/package.json).
 */
class SettingsParityTest {
    private fun flatten(o: JsonObject, prefix: String = ""): Set<String> = o.entrySet().flatMap { (k, v) ->
        val key = if (prefix.isEmpty()) k else "$prefix.$k"
        // maps (urls/localPaths) are leaf values
        if (v.isJsonObject && k !in setOf("urls", "localPaths")) flatten(v.asJsonObject, key) else listOf(key)
    }.toSet()

    @Test
    fun intellijSettingsMatchVsCode() {
        val pkg = JsonParser.parseString(File("../vscode/package.json").readText()).asJsonObject
        val vscodeKeys = pkg.getAsJsonObject("contributes").getAsJsonObject("configuration").getAsJsonObject("properties")
            .keySet().map { it.removePrefix("azure-templates-navigator.") }
            .filter { !it.startsWith("server.") } // client-only
            .toSet()
        val intellijKeys = flatten(AtnSettings().toServerJson())
        assertEquals(vscodeKeys.sorted(), intellijKeys.sorted())
    }
}
