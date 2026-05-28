import * as fs from "fs";
import * as path from "path";
import * as os from "os";
import { execSync } from "child_process";
function isProcessRunning(pattern) {
    try {
        const out = execSync(`pgrep -f '${pattern}' 2>/dev/null`, { encoding: "utf-8", timeout: 3000 });
        return out.trim().length > 0;
    } catch { return false; }
}
function hasOpenClaw() {
    const dir = path.join(os.homedir(), ".openclaw");
    if (!fs.existsSync(dir)) return false;
    const config = path.join(dir, "openclaw.json");
    if (fs.existsSync(config)) return true;
    try {
        return fs.readdirSync(dir).some((e) => e.startsWith("workspace-"));
    } catch { return false; }
}
function hasHermes() {
    return fs.existsSync(path.join(os.homedir(), ".hermes", "config.yaml"));
}
export function detectFramework(override) {
    if (override && (override === "hermes" || override === "openclaw")) {
        const dir = path.join(os.homedir(), override === "hermes" ? ".hermes" : ".openclaw");
        return { framework: override, configPath: dir, workspacePath: dir };
    }
    const oc = hasOpenClaw();
    const hm = hasHermes();
    // If both exist, prefer the one with a running process
    if (oc && hm) {
        const ocRunning = isProcessRunning("openclaw") || isProcessRunning("openclaw-gateway");
        const hmRunning = isProcessRunning("hermes.*gateway") || isProcessRunning("hermes-gateway");
        if (ocRunning && !hmRunning) {
            return makeOpenClaw();
        }
        if (hmRunning && !ocRunning) {
            return makeHermes();
        }
        // Both running or neither running -- prefer openclaw (more common)
        return makeOpenClaw();
    }
    if (oc) return makeOpenClaw();
    if (hm) return makeHermes();
    console.error("No supported agent framework detected.\n" +
        "Looked for:\n" +
        "  - OpenClaw: ~/.openclaw/ (with openclaw.json or workspace-* dirs)\n" +
        "  - Hermes:   ~/.hermes/config.yaml\n\n" +
        "Use --framework openclaw|hermes to specify manually.");
    process.exit(1);
}
function makeOpenClaw() {
    const dir = path.join(os.homedir(), ".openclaw");
    const configPath = path.join(dir, "openclaw.json");
    return { framework: "openclaw", configPath: fs.existsSync(configPath) ? configPath : dir, workspacePath: dir };
}
function makeHermes() {
    const configPath = path.join(os.homedir(), ".hermes", "config.yaml");
    return { framework: "hermes", configPath, workspacePath: path.join(os.homedir(), ".hermes") };
}
