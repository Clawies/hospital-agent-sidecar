import * as fs from "fs";
import * as path from "path";
import * as os from "os";
export function detectFramework(override) {
    if (override && (override === "hermes" || override === "openclaw")) {
        const dir = path.join(os.homedir(), override === "hermes" ? ".hermes" : ".openclaw");
        return { framework: override, configPath: dir, workspacePath: dir };
    }
    const hermesConfig = path.join(os.homedir(), ".hermes", "config.yaml");
    if (fs.existsSync(hermesConfig)) {
        return {
            framework: "hermes",
            configPath: hermesConfig,
            workspacePath: path.join(os.homedir(), ".hermes"),
        };
    }
    const openclawDir = path.join(os.homedir(), ".openclaw");
    if (fs.existsSync(openclawDir)) {
        const configPath = path.join(openclawDir, "openclaw.json");
        if (fs.existsSync(configPath)) {
            return { framework: "openclaw", configPath, workspacePath: openclawDir };
        }
        try {
            const entries = fs.readdirSync(openclawDir);
            if (entries.some((e) => e.startsWith("workspace-"))) {
                return { framework: "openclaw", configPath: openclawDir, workspacePath: openclawDir };
            }
        }
        catch { }
    }
    console.error("No supported agent framework detected.\n" +
        "Looked for:\n" +
        "  - Hermes:   ~/.hermes/config.yaml\n" +
        "  - OpenClaw: ~/.openclaw/ (with openclaw.json or workspace-* dirs)\n\n" +
        "Use --framework openclaw|hermes to specify manually.");
    process.exit(1);
}
