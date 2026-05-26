import * as fs from "fs";
import * as path from "path";
import * as os from "os";
import * as net from "net";
import * as http from "http";
import * as https from "https";
import { execSync } from "child_process";
// ---------------------------------------------------------------------------
// Shared utilities
// ---------------------------------------------------------------------------
function checkPort(port) {
    return new Promise((resolve) => {
        const sock = net.createConnection({ port, host: "127.0.0.1" }, () => {
            sock.destroy();
            resolve(true);
        });
        sock.on("error", () => resolve(false));
        sock.setTimeout(2000, () => {
            sock.destroy();
            resolve(false);
        });
    });
}
function dirSize(dirPath) {
    let total = 0;
    try {
        const entries = fs.readdirSync(dirPath, { withFileTypes: true });
        for (const entry of entries) {
            const full = path.join(dirPath, entry.name);
            if (entry.isFile()) {
                try {
                    total += fs.statSync(full).size;
                }
                catch { }
            }
            else if (entry.isDirectory()) {
                total += dirSize(full);
            }
        }
    }
    catch { }
    return total;
}
function bytesToMb(bytes) {
    return Math.round((bytes / (1024 * 1024)) * 10) / 10;
}
function getSafeConfigSnapshot(config) {
    const snapshot = JSON.parse(JSON.stringify(config || {}));
    const redact = (obj, depth = 0) => {
        if (depth > 10 || !obj || typeof obj !== "object")
            return;
        for (const key of Object.keys(obj)) {
            const lk = key.toLowerCase();
            if (lk.includes("key") || lk.includes("token") || lk.includes("secret") || lk.includes("password") || lk.includes("credential")) {
                if (typeof obj[key] === "string" && obj[key].length > 0) {
                    obj[key] = "[REDACTED]";
                }
            }
            else if (typeof obj[key] === "object") {
                redact(obj[key], depth + 1);
            }
        }
    };
    redact(snapshot);
    return snapshot;
}
// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------
function httpGet(port, urlPath, timeoutMs = 5000) {
    return new Promise((resolve, reject) => {
        const start = Date.now();
        const req = http.request({ hostname: "127.0.0.1", port, path: urlPath, method: "GET", timeout: timeoutMs }, (res) => {
            let data = "";
            res.on("data", (chunk) => { data += chunk.toString(); });
            res.on("end", () => {
                resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 2000), latencyMs: Date.now() - start });
            });
        });
        req.on("error", (err) => reject(err));
        req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
        req.end();
    });
}
async function probeGateway(port) {
    const start = Date.now();
    try {
        const res = await httpGet(port, "/health");
        return {
            probe: "gateway_http",
            success: res.statusCode >= 200 && res.statusCode < 500,
            latencyMs: res.latencyMs,
            output: `HTTP ${res.statusCode}: ${res.body.slice(0, 500)}`,
            error: res.statusCode >= 500 ? `Server error ${res.statusCode}` : null,
        };
    }
    catch (err) {
        return {
            probe: "gateway_http",
            success: false,
            latencyMs: Date.now() - start,
            output: "",
            error: err.message || "Connection failed",
        };
    }
}
async function probeModelApi(config) {
    const start = Date.now();
    const envConfig = config?.env || {};
    let apiKey = null;
    let baseUrl = null;
    let provider = "unknown";
    if (envConfig.OPENROUTER_API_KEY || process.env.OPENROUTER_API_KEY) {
        apiKey = envConfig.OPENROUTER_API_KEY || process.env.OPENROUTER_API_KEY || null;
        baseUrl = "https://openrouter.ai/api/v1";
        provider = "openrouter";
    }
    else if (envConfig.ANTHROPIC_API_KEY || process.env.ANTHROPIC_API_KEY) {
        apiKey = envConfig.ANTHROPIC_API_KEY || process.env.ANTHROPIC_API_KEY || null;
        baseUrl = "https://api.anthropic.com";
        provider = "anthropic";
    }
    else if (envConfig.OPENAI_API_KEY || process.env.OPENAI_API_KEY) {
        apiKey = envConfig.OPENAI_API_KEY || process.env.OPENAI_API_KEY || null;
        baseUrl = "https://api.openai.com/v1";
        provider = "openai";
    }
    else if (config?.models?.providers) {
        const providerNames = Object.keys(config.models.providers);
        if (providerNames.length > 0) {
            provider = providerNames[0];
            const providerConfig = config.models.providers[provider];
            apiKey = providerConfig?.apiKey || null;
            baseUrl = providerConfig?.baseUrl || null;
        }
    }
    if (!apiKey || !baseUrl) {
        return {
            probe: "model_api",
            success: false,
            latencyMs: Date.now() - start,
            output: `provider=${provider}, no API key or base URL configured`,
            error: "No model API credentials found",
        };
    }
    try {
        if (provider === "anthropic") {
            const testUrl = `${baseUrl}/v1/messages`;
            const body = JSON.stringify({ model: "claude-sonnet-4-20250514", max_tokens: 1, messages: [{ role: "user", content: "hi" }] });
            const result = await new Promise((resolve, reject) => {
                const url = new URL(testUrl);
                const req = https.request({
                    hostname: url.hostname,
                    port: 443,
                    path: url.pathname,
                    method: "POST",
                    headers: {
                        "x-api-key": apiKey,
                        "anthropic-version": "2023-06-01",
                        "content-type": "application/json",
                        "content-length": Buffer.byteLength(body).toString(),
                    },
                    timeout: 10000,
                }, (res) => {
                    let data = "";
                    res.on("data", (chunk) => { data += chunk.toString(); });
                    res.on("end", () => resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 500), latencyMs: Date.now() - start }));
                });
                req.on("error", reject);
                req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
                req.write(body);
                req.end();
            });
            const success = result.statusCode >= 200 && result.statusCode < 500;
            return {
                probe: "model_api",
                success,
                latencyMs: result.latencyMs,
                output: `provider=${provider}, HTTP ${result.statusCode}, latency=${result.latencyMs}ms`,
                error: result.statusCode >= 400 ? `API error ${result.statusCode}: ${result.body.slice(0, 200)}` : null,
            };
        }
        else {
            const testUrl = `${baseUrl}/models`;
            const url = new URL(testUrl);
            const transport = url.protocol === "https:" ? https : http;
            const result = await new Promise((resolve, reject) => {
                const req = transport.request({
                    hostname: url.hostname,
                    port: url.port || (url.protocol === "https:" ? 443 : 80),
                    path: url.pathname,
                    method: "GET",
                    headers: { "Authorization": `Bearer ${apiKey}` },
                    timeout: 10000,
                }, (res) => {
                    let data = "";
                    res.on("data", (chunk) => { data += chunk.toString(); });
                    res.on("end", () => resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 500), latencyMs: Date.now() - start }));
                });
                req.on("error", reject);
                req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
                req.end();
            });
            const success = result.statusCode >= 200 && result.statusCode < 400;
            return {
                probe: "model_api",
                success,
                latencyMs: result.latencyMs,
                output: `provider=${provider}, HTTP ${result.statusCode}, latency=${result.latencyMs}ms`,
                error: result.statusCode >= 400 ? `API error ${result.statusCode}: ${result.body.slice(0, 200)}` : null,
            };
        }
    }
    catch (err) {
        return {
            probe: "model_api",
            success: false,
            latencyMs: Date.now() - start,
            output: `provider=${provider}`,
            error: err.message || "Request failed",
        };
    }
}
function probeProcessHealth(pid) {
    const start = Date.now();
    if (!pid) {
        return {
            probe: "process_health",
            success: false,
            latencyMs: Date.now() - start,
            output: "No PID found",
            error: "Process not running",
        };
    }
    try {
        process.kill(pid, 0);
        const platform = os.platform();
        let output = "";
        if (platform === "linux" || platform === "darwin") {
            try {
                const psOutput = execSync(`ps -p ${pid} -o rss=,pcpu=,vsz=,etime= 2>/dev/null`, {
                    encoding: "utf-8",
                    timeout: 5000,
                }).trim();
                const parts = psOutput.trim().split(/\s+/);
                const rssKb = parseInt(parts[0] || "0", 10);
                const cpuPercent = parseFloat(parts[1] || "0");
                const vszKb = parseInt(parts[2] || "0", 10);
                const elapsed = parts[3] || "unknown";
                output = `pid=${pid}, rss=${Math.round(rssKb / 1024)}MB, vsz=${Math.round(vszKb / 1024)}MB, cpu=${cpuPercent}%, elapsed=${elapsed}`;
            }
            catch {
                output = `pid=${pid}, alive=true (ps stats unavailable)`;
            }
            if (platform === "linux") {
                try {
                    const fdCount = execSync(`ls /proc/${pid}/fd 2>/dev/null | wc -l`, {
                        encoding: "utf-8",
                        timeout: 5000,
                    }).trim();
                    output += `, openFDs=${fdCount}`;
                }
                catch { }
            }
        }
        else {
            output = `pid=${pid}, alive=true`;
        }
        return {
            probe: "process_health",
            success: true,
            latencyMs: Date.now() - start,
            output,
            error: null,
        };
    }
    catch {
        return {
            probe: "process_health",
            success: false,
            latencyMs: Date.now() - start,
            output: `pid=${pid}`,
            error: "Process not found (may have crashed)",
        };
    }
}
function probeSqliteIntegrity(framework) {
    const start = Date.now();
    const homeDir = os.homedir();
    let dbPaths = [];
    if (framework === "hermes") {
        dbPaths = [path.join(homeDir, ".hermes", "state.db")];
    }
    else {
        const openclawDir = path.join(homeDir, ".openclaw");
        try {
            const entries = fs.readdirSync(openclawDir);
            dbPaths = entries
                .filter((e) => e.endsWith(".db") || e.endsWith(".sqlite"))
                .map((e) => path.join(openclawDir, e));
        }
        catch { }
    }
    if (dbPaths.length === 0) {
        return {
            probe: "sqlite_integrity",
            success: true,
            latencyMs: Date.now() - start,
            output: "No SQLite databases found",
            error: null,
        };
    }
    const results = [];
    let allOk = true;
    for (const dbPath of dbPaths) {
        if (!fs.existsSync(dbPath))
            continue;
        try {
            const output = execSync(`sqlite3 "${dbPath}" "PRAGMA integrity_check; PRAGMA page_count; PRAGMA freelist_count;"`, { encoding: "utf-8", timeout: 10000 }).trim();
            const lines = output.split("\n");
            const integrityOk = lines[0] === "ok";
            if (!integrityOk)
                allOk = false;
            results.push(`${path.basename(dbPath)}: integrity=${lines[0]}, pages=${lines[1] || "?"}, freelist=${lines[2] || "?"}`);
        }
        catch (err) {
            allOk = false;
            results.push(`${path.basename(dbPath)}: error=${err.message?.slice(0, 100) || "failed"}`);
        }
    }
    return {
        probe: "sqlite_integrity",
        success: allOk,
        latencyMs: Date.now() - start,
        output: results.join("; "),
        error: allOk ? null : "SQLite integrity issues detected",
    };
}
function probeCronValidity(crons) {
    const start = Date.now();
    if (crons.length === 0) {
        return {
            probe: "cron_validity",
            success: true,
            latencyMs: Date.now() - start,
            output: "No cron jobs configured",
            error: null,
        };
    }
    const results = [];
    let allOk = true;
    for (const cron of crons) {
        const fields = cron.expression.trim().split(/\s+/);
        if (fields.length < 5 || fields.length > 6) {
            allOk = false;
            results.push(`${cron.id}: INVALID expression "${cron.expression}" (${fields.length} fields, expected 5-6)`);
            continue;
        }
        if (cron.nextRun) {
            const nextRunDate = new Date(cron.nextRun);
            const now = new Date();
            if (nextRunDate.getTime() < now.getTime() - 300000) {
                allOk = false;
                results.push(`${cron.id}: STALE nextRun=${cron.nextRun} is in the past (scheduler may be dead)`);
                continue;
            }
        }
        results.push(`${cron.id}: OK (${cron.expression})`);
    }
    return {
        probe: "cron_validity",
        success: allOk,
        latencyMs: Date.now() - start,
        output: results.join("; "),
        error: allOk ? null : "Cron job issues detected",
    };
}
function probeErrorCount(framework) {
    const start = Date.now();
    const homeDir = os.homedir();
    const frameworkDir = framework === "hermes"
        ? path.join(homeDir, ".hermes")
        : path.join(homeDir, ".openclaw");
    let errorCount = 0;
    let logFile = "";
    const recentErrors = [];
    try {
        const logsDir = path.join(frameworkDir, "logs");
        if (!fs.existsSync(logsDir)) {
            return {
                probe: "error_count",
                success: true,
                latencyMs: Date.now() - start,
                output: "No logs directory found",
                error: null,
            };
        }
        const files = fs.readdirSync(logsDir)
            .filter((f) => f.endsWith(".log") || f.endsWith(".jsonl"))
            .map((f) => {
            try {
                return { name: f, mtime: fs.statSync(path.join(logsDir, f)).mtime };
            }
            catch {
                return null;
            }
        })
            .filter((f) => f !== null)
            .sort((a, b) => b.mtime.getTime() - a.mtime.getTime());
        if (files.length === 0) {
            return {
                probe: "error_count",
                success: true,
                latencyMs: Date.now() - start,
                output: "No log files found",
                error: null,
            };
        }
        logFile = files[0].name;
        const logPath = path.join(logsDir, logFile);
        const stat = fs.statSync(logPath);
        const readSize = Math.min(stat.size, 32768);
        const buf = Buffer.alloc(readSize);
        const fd = fs.openSync(logPath, "r");
        fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
        fs.closeSync(fd);
        const content = buf.toString("utf-8");
        const lines = content.split("\n");
        const errorPatterns = [/ERROR/i, /FATAL/i, /PANIC/i, /CRASH/i, /Exception/i, /Traceback/i, /ECONNREFUSED/, /ENOSPC/, /ENOMEM/];
        for (const line of lines) {
            for (const pattern of errorPatterns) {
                if (pattern.test(line)) {
                    errorCount++;
                    if (recentErrors.length < 5) {
                        recentErrors.push(line.trim().slice(0, 200));
                    }
                    break;
                }
            }
        }
    }
    catch (err) {
        return {
            probe: "error_count",
            success: false,
            latencyMs: Date.now() - start,
            output: "",
            error: err.message || "Failed to read logs",
        };
    }
    const output = `file=${logFile}, errors=${errorCount}${recentErrors.length > 0 ? ", recent: " + recentErrors.join(" | ") : ""}`;
    return {
        probe: "error_count",
        success: errorCount < 10,
        latencyMs: Date.now() - start,
        output: output.slice(0, 2000),
        error: errorCount >= 10 ? `High error count (${errorCount}) in recent logs` : null,
    };
}
async function probeIntegrationConnectivity(config) {
    const results = [];
    const envConfig = config?.env || {};
    const channels = config?.channels || {};
    // Slack
    const slackToken = envConfig.SLACK_BOT_TOKEN || envConfig.SLACK_TOKEN || process.env.SLACK_BOT_TOKEN;
    if (slackToken || channels.slack?.enabled) {
        const start = Date.now();
        if (slackToken) {
            try {
                const result = await new Promise((resolve, reject) => {
                    const req = https.request({
                        hostname: "slack.com",
                        path: "/api/auth.test",
                        method: "POST",
                        headers: { "Authorization": `Bearer ${slackToken}`, "Content-Type": "application/x-www-form-urlencoded" },
                        timeout: 8000,
                    }, (res) => {
                        let data = "";
                        res.on("data", (chunk) => { data += chunk.toString(); });
                        res.on("end", () => resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 300) }));
                    });
                    req.on("error", reject);
                    req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
                    req.end();
                });
                const bodyParsed = JSON.parse(result.body);
                results.push({
                    probe: "integration_slack",
                    success: bodyParsed.ok === true,
                    latencyMs: Date.now() - start,
                    output: bodyParsed.ok ? `connected as ${bodyParsed.user || "unknown"}` : `error: ${bodyParsed.error || "unknown"}`,
                    error: bodyParsed.ok ? null : bodyParsed.error || "auth failed",
                });
            }
            catch (err) {
                results.push({ probe: "integration_slack", success: false, latencyMs: Date.now() - start, output: "", error: err.message });
            }
        }
        else {
            results.push({ probe: "integration_slack", success: false, latencyMs: 0, output: "enabled but no token found", error: "missing SLACK_BOT_TOKEN" });
        }
    }
    // Telegram
    const telegramToken = envConfig.TELEGRAM_BOT_TOKEN || envConfig.TELEGRAM_TOKEN || process.env.TELEGRAM_BOT_TOKEN;
    if (telegramToken || channels.telegram?.enabled) {
        const start = Date.now();
        if (telegramToken) {
            try {
                const result = await new Promise((resolve, reject) => {
                    const req = https.request({
                        hostname: "api.telegram.org",
                        path: `/bot${telegramToken}/getMe`,
                        method: "GET",
                        timeout: 8000,
                    }, (res) => {
                        let data = "";
                        res.on("data", (chunk) => { data += chunk.toString(); });
                        res.on("end", () => resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 300) }));
                    });
                    req.on("error", reject);
                    req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
                    req.end();
                });
                const bodyParsed = JSON.parse(result.body);
                results.push({
                    probe: "integration_telegram",
                    success: bodyParsed.ok === true,
                    latencyMs: Date.now() - start,
                    output: bodyParsed.ok ? `bot: @${bodyParsed.result?.username || "unknown"}` : `error: ${bodyParsed.description || "unknown"}`,
                    error: bodyParsed.ok ? null : bodyParsed.description || "auth failed",
                });
            }
            catch (err) {
                results.push({ probe: "integration_telegram", success: false, latencyMs: Date.now() - start, output: "", error: err.message });
            }
        }
        else {
            results.push({ probe: "integration_telegram", success: false, latencyMs: 0, output: "enabled but no token found", error: "missing TELEGRAM_BOT_TOKEN" });
        }
    }
    // Discord
    const discordToken = envConfig.DISCORD_BOT_TOKEN || envConfig.DISCORD_TOKEN || process.env.DISCORD_BOT_TOKEN;
    if (discordToken || channels.discord?.enabled) {
        const start = Date.now();
        if (discordToken) {
            try {
                const result = await new Promise((resolve, reject) => {
                    const req = https.request({
                        hostname: "discord.com",
                        path: "/api/v10/users/@me",
                        method: "GET",
                        headers: { "Authorization": `Bot ${discordToken}` },
                        timeout: 8000,
                    }, (res) => {
                        let data = "";
                        res.on("data", (chunk) => { data += chunk.toString(); });
                        res.on("end", () => resolve({ statusCode: res.statusCode ?? 0, body: data.slice(0, 300) }));
                    });
                    req.on("error", reject);
                    req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
                    req.end();
                });
                const success = result.statusCode >= 200 && result.statusCode < 400;
                results.push({
                    probe: "integration_discord",
                    success,
                    latencyMs: Date.now() - start,
                    output: success ? `HTTP ${result.statusCode}` : `HTTP ${result.statusCode}: ${result.body.slice(0, 100)}`,
                    error: success ? null : `HTTP ${result.statusCode}`,
                });
            }
            catch (err) {
                results.push({ probe: "integration_discord", success: false, latencyMs: Date.now() - start, output: "", error: err.message });
            }
        }
        else {
            results.push({ probe: "integration_discord", success: false, latencyMs: 0, output: "enabled but no token found", error: "missing DISCORD_BOT_TOKEN" });
        }
    }
    return results;
}
async function runAllProbes(framework, gatewayPort, pid, config, crons) {
    const results = [];
    const [gatewayResult, modelResult, integrationResults] = await Promise.all([
        probeGateway(gatewayPort),
        probeModelApi(config),
        probeIntegrationConnectivity(config),
    ]);
    results.push(gatewayResult);
    results.push(modelResult);
    results.push(...integrationResults);
    results.push(probeProcessHealth(pid));
    results.push(probeSqliteIntegrity(framework));
    results.push(probeCronValidity(crons));
    results.push(probeErrorCount(framework));
    return results;
}
// ---------------------------------------------------------------------------
// OpenClaw health collection
// ---------------------------------------------------------------------------
const OPENCLAW_DIR = path.join(os.homedir(), ".openclaw");
const OPENCLAW_CONFIG_PATH = path.join(OPENCLAW_DIR, "openclaw.json");
const OPENCLAW_DAEMON_PORT = 18789;
function readOpenClawConfig() {
    try {
        const raw = fs.readFileSync(OPENCLAW_CONFIG_PATH, "utf-8");
        return JSON.parse(raw);
    }
    catch {
        try {
            const raw = fs.readFileSync(OPENCLAW_CONFIG_PATH, "utf-8");
            const cleaned = raw
                .replace(/\/\/.*$/gm, "")
                .replace(/\/\*[\s\S]*?\*\//g, "")
                .replace(/,\s*([}\]])/g, "$1");
            return JSON.parse(cleaned);
        }
        catch {
            return {};
        }
    }
}
function findOpenClawWorkspaces() {
    const workspaces = [];
    const singleWs = path.join(OPENCLAW_DIR, "workspace");
    if (fs.existsSync(singleWs) && fs.statSync(singleWs).isDirectory()) {
        workspaces.push(singleWs);
    }
    try {
        const entries = fs.readdirSync(OPENCLAW_DIR);
        for (const e of entries) {
            if (e.startsWith("workspace-")) {
                const full = path.join(OPENCLAW_DIR, e);
                if (fs.statSync(full).isDirectory()) {
                    workspaces.push(full);
                }
            }
        }
    }
    catch { }
    return workspaces;
}
function findOpenClawPid() {
    const pidFile = path.join(OPENCLAW_DIR, "daemon.pid");
    try {
        const pid = parseInt(fs.readFileSync(pidFile, "utf-8").trim(), 10);
        if (!isNaN(pid)) {
            try {
                process.kill(pid, 0);
                return pid;
            }
            catch {
                return null;
            }
        }
    }
    catch { }
    return null;
}
function getOpenClawPersonality(workspaces) {
    for (const ws of workspaces) {
        const soulPath = path.join(ws, "SOUL.md");
        try {
            const stat = fs.statSync(soulPath);
            const content = fs.readFileSync(soulPath, "utf-8");
            const words = content.split(/\s+/).filter((w) => w.length > 0).length;
            return { file: "SOUL.md", exists: true, lastModified: stat.mtime.toISOString(), wordCount: words };
        }
        catch { }
    }
    return { file: "SOUL.md", exists: false, lastModified: null, wordCount: 0 };
}
function getOpenClawWorkspaceFileInfo(workspaces, fileName) {
    for (const ws of workspaces) {
        const filePath = path.join(ws, fileName);
        try {
            const stat = fs.statSync(filePath);
            const content = fs.readFileSync(filePath, "utf-8");
            const words = content.split(/\s+/).filter((w) => w.length > 0).length;
            const lines = content.split("\n").length;
            return { exists: true, wordCount: words, lineCount: lines, sizeBytes: stat.size, lastModified: stat.mtime.toISOString() };
        }
        catch { }
    }
    return { exists: false, wordCount: 0, lineCount: 0, sizeBytes: 0, lastModified: null };
}
function getOpenClawMemory(workspaces) {
    let sizeBytes = 0;
    let entryCount = 0;
    let storeReachable = false;
    let lastWrite = null;
    for (const ws of workspaces) {
        const memDir = path.join(ws, "memory");
        try {
            const entries = fs.readdirSync(memDir);
            storeReachable = true;
            entryCount += entries.length;
            sizeBytes += dirSize(memDir);
            for (const e of entries) {
                try {
                    const stat = fs.statSync(path.join(memDir, e));
                    const mtime = stat.mtime.toISOString();
                    if (!lastWrite || mtime > lastWrite)
                        lastWrite = mtime;
                }
                catch { }
            }
        }
        catch { }
    }
    const memoryMd = getOpenClawWorkspaceFileInfo(workspaces, "MEMORY.md");
    if (memoryMd.exists) {
        storeReachable = true;
        entryCount += memoryMd.lineCount;
        sizeBytes += memoryMd.sizeBytes;
        if (memoryMd.lastModified && (!lastWrite || memoryMd.lastModified > lastWrite)) {
            lastWrite = memoryMd.lastModified;
        }
    }
    const globalMemDir = path.join(OPENCLAW_DIR, "memory");
    try {
        const entries = fs.readdirSync(globalMemDir);
        storeReachable = true;
        entryCount += entries.length;
        sizeBytes += dirSize(globalMemDir);
    }
    catch { }
    return { type: storeReachable ? "file" : "none", entryCount, storeReachable, lastWrite, sizeBytes };
}
function getOpenClawIntegrations(config) {
    const integrations = [];
    const channels = config?.channels || {};
    for (const [platform, settings] of Object.entries(channels)) {
        integrations.push({
            type: platform,
            connected: !!settings?.enabled,
            tokenExpiresAt: null,
            lastMessageAt: null,
            error: null,
        });
    }
    return integrations;
}
function getOpenClawCrons() {
    const crons = [];
    const cronFile = path.join(OPENCLAW_DIR, "cron", "jobs.json");
    try {
        const raw = fs.readFileSync(cronFile, "utf-8");
        const data = JSON.parse(raw);
        const jobs = Array.isArray(data) ? data : (data.jobs || []);
        for (const j of jobs) {
            crons.push({
                id: j.id || j.name || j.label || "unnamed",
                expression: j.expression || j.schedule || j.cron || "",
                timezone: j.timezone || null,
                lastRun: j.lastRun || null,
                lastStatus: j.lastStatus || "unknown",
                lastError: j.lastError || null,
                nextRun: j.nextRun || null,
                deliveryTarget: j.to || j.target || null,
            });
        }
    }
    catch { }
    return crons;
}
function getOpenClawModel(config) {
    const envConfig = config?.env || {};
    let provider = "unknown";
    let apiReachable = false;
    if (envConfig.OPENROUTER_API_KEY || process.env.OPENROUTER_API_KEY) {
        provider = "openrouter";
        apiReachable = true;
    }
    else if (envConfig.ANTHROPIC_API_KEY || process.env.ANTHROPIC_API_KEY) {
        provider = "anthropic";
        apiReachable = true;
    }
    else if (envConfig.OPENAI_API_KEY || process.env.OPENAI_API_KEY) {
        provider = "openai";
        apiReachable = true;
    }
    else if (config?.models?.providers) {
        const providerNames = Object.keys(config.models.providers);
        if (providerNames.length > 0) {
            provider = providerNames[0];
            const providerConfig = config.models.providers[provider];
            apiReachable = !!(providerConfig?.apiKey || providerConfig?.baseUrl);
        }
    }
    const agentModel = config?.agents?.defaults?.model;
    let modelId = (typeof agentModel === "string" ? agentModel : agentModel?.primary)
        || config?.agents?.main?.model
        || config?.model?.name
        || "unknown";
    if (modelId.startsWith(`${provider}/`)) {
        modelId = modelId.slice(provider.length + 1);
    }
    return { provider, modelId, apiReachable, authValid: apiReachable, latencyMs: null };
}
function getOpenClawDisk(workspaces) {
    const totalBytes = dirSize(OPENCLAW_DIR);
    let sessionsBytes = dirSize(path.join(OPENCLAW_DIR, "completions"));
    for (const ws of workspaces) {
        try {
            const entries = fs.readdirSync(ws);
            for (const e of entries) {
                if (e.endsWith(".jsonl")) {
                    try {
                        sessionsBytes += fs.statSync(path.join(ws, e)).size;
                    }
                    catch { }
                }
            }
        }
        catch { }
    }
    const logsBytes = dirSize(path.join(OPENCLAW_DIR, "logs"));
    return { homeDirMb: bytesToMb(totalBytes), sessionsMb: bytesToMb(sessionsBytes), logsMb: bytesToMb(logsBytes), totalMb: bytesToMb(totalBytes) };
}
function getOpenClawLogsTail() {
    const logsDir = path.join(OPENCLAW_DIR, "logs");
    try {
        const files = fs.readdirSync(logsDir)
            .filter((f) => f.endsWith(".log"))
            .map((f) => ({ name: f, mtime: fs.statSync(path.join(logsDir, f)).mtime }))
            .sort((a, b) => b.mtime.getTime() - a.mtime.getTime());
        if (files.length > 0) {
            const logPath = path.join(logsDir, files[0].name);
            const stat = fs.statSync(logPath);
            const readSize = Math.min(stat.size, 4096);
            const buf = Buffer.alloc(readSize);
            const fd = fs.openSync(logPath, "r");
            fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
            fs.closeSync(fd);
            return buf.toString("utf-8");
        }
    }
    catch { }
    return null;
}
function getOpenClawSkillValidation(workspaces, soulContent) {
    const validations = [];
    const soulLower = (soulContent || "").toLowerCase();
    for (const ws of workspaces) {
        const skillsDir = path.join(ws, "skills");
        try {
            const entries = fs.readdirSync(skillsDir, { withFileTypes: true });
            for (const entry of entries) {
                if (!entry.isDirectory())
                    continue;
                const skillDir = path.join(skillsDir, entry.name);
                let hasDefinition = false;
                let isEmpty = true;
                try {
                    const skillFiles = fs.readdirSync(skillDir);
                    for (const sf of skillFiles) {
                        if (sf.endsWith(".md") || sf.endsWith(".json") || sf.endsWith(".yaml") || sf.endsWith(".yml")) {
                            hasDefinition = true;
                            try {
                                const stat = fs.statSync(path.join(skillDir, sf));
                                if (stat.size > 10)
                                    isEmpty = false;
                            }
                            catch { }
                        }
                    }
                }
                catch { }
                const referencedInSoul = soulLower.includes(entry.name.toLowerCase());
                validations.push({ name: entry.name, hasDefinition, isEmpty: hasDefinition ? isEmpty : true, referencedInSoul });
            }
            if (validations.length > 0)
                break;
        }
        catch { }
    }
    return validations;
}
function getOpenClawWorkspaceDetails(workspaces) {
    const files = {};
    const fileNames = ["SOUL.md", "MEMORY.md", "TOOLS.md", "USER.md", "IDENTITY.md", "AGENTS.md", "HEARTBEAT.md"];
    for (const fname of fileNames) {
        const key = fname.replace(".md", "").toLowerCase();
        files[key] = { exists: false, wordCount: 0, lineCount: 0, sizeBytes: 0, lastModified: null };
        for (const ws of workspaces) {
            const filePath = path.join(ws, fname);
            try {
                const stat = fs.statSync(filePath);
                const content = fs.readFileSync(filePath, "utf-8");
                const words = content.split(/\s+/).filter((w) => w.length > 0).length;
                const lines = content.split("\n").length;
                files[key] = { exists: true, wordCount: words, lineCount: lines, sizeBytes: stat.size, lastModified: stat.mtime.toISOString() };
                break;
            }
            catch { }
        }
    }
    const soulContent = files["soul"]?.exists ? (() => {
        for (const ws of workspaces) {
            try {
                return fs.readFileSync(path.join(ws, "SOUL.md"), "utf-8");
            }
            catch { }
        }
        return null;
    })() : null;
    const skillValidation = getOpenClawSkillValidation(workspaces, soulContent);
    let skills = { count: 0, names: [], validation: skillValidation };
    for (const ws of workspaces) {
        const skillsDir = path.join(ws, "skills");
        try {
            const entries = fs.readdirSync(skillsDir, { withFileTypes: true });
            const names = entries
                .filter((e) => e.isDirectory() || e.name.endsWith(".md") || e.name.endsWith(".json"))
                .map((e) => e.name);
            skills = { count: names.length, names, validation: skillValidation };
            break;
        }
        catch { }
    }
    let pipeline = { exists: false, stageCount: 0 };
    for (const ws of workspaces) {
        const pipelineDir = path.join(ws, "pipeline");
        try {
            const entries = fs.readdirSync(pipelineDir);
            pipeline = { exists: true, stageCount: entries.length };
            break;
        }
        catch { }
    }
    return { files, skills, pipeline };
}
function getOpenClawSessionHealth(workspaces) {
    let totalCount = 0;
    let oldestTimestamp = null;
    let newestTimestamp = null;
    let olderThan30Days = 0;
    let totalSizeBytes = 0;
    const thirtyDaysAgo = Date.now() - 30 * 24 * 60 * 60 * 1000;
    const completionsDir = path.join(OPENCLAW_DIR, "completions");
    try {
        const entries = fs.readdirSync(completionsDir, { withFileTypes: true });
        for (const entry of entries) {
            if (!entry.isFile())
                continue;
            const full = path.join(completionsDir, entry.name);
            try {
                const stat = fs.statSync(full);
                totalCount++;
                totalSizeBytes += stat.size;
                const mtime = stat.mtime.toISOString();
                if (!oldestTimestamp || mtime < oldestTimestamp)
                    oldestTimestamp = mtime;
                if (!newestTimestamp || mtime > newestTimestamp)
                    newestTimestamp = mtime;
                if (stat.mtime.getTime() < thirtyDaysAgo)
                    olderThan30Days++;
            }
            catch { }
        }
    }
    catch { }
    for (const ws of workspaces) {
        try {
            const entries = fs.readdirSync(ws);
            for (const e of entries) {
                if (!e.endsWith(".jsonl"))
                    continue;
                const full = path.join(ws, e);
                try {
                    const stat = fs.statSync(full);
                    totalCount++;
                    totalSizeBytes += stat.size;
                    const mtime = stat.mtime.toISOString();
                    if (!oldestTimestamp || mtime < oldestTimestamp)
                        oldestTimestamp = mtime;
                    if (!newestTimestamp || mtime > newestTimestamp)
                        newestTimestamp = mtime;
                    if (stat.mtime.getTime() < thirtyDaysAgo)
                        olderThan30Days++;
                }
                catch { }
            }
        }
        catch { }
    }
    return { totalCount, oldestTimestamp, newestTimestamp, olderThan30Days, totalSizeMb: bytesToMb(totalSizeBytes) };
}
function getOpenClawLogAnalysis() {
    const logsDir = path.join(OPENCLAW_DIR, "logs");
    let totalErrors = 0;
    let totalWarnings = 0;
    const errorCounts = new Map();
    try {
        const files = fs.readdirSync(logsDir)
            .filter((f) => f.endsWith(".log"))
            .map((f) => {
            try {
                return { name: f, mtime: fs.statSync(path.join(logsDir, f)).mtime };
            }
            catch {
                return null;
            }
        })
            .filter((f) => f !== null)
            .sort((a, b) => b.mtime.getTime() - a.mtime.getTime());
        if (files.length === 0) {
            return { totalErrors: 0, totalWarnings: 0, topErrors: [], errorRate: "no log files" };
        }
        const logPath = path.join(logsDir, files[0].name);
        const stat = fs.statSync(logPath);
        const readSize = Math.min(stat.size, 32768);
        const buf = Buffer.alloc(readSize);
        const fd = fs.openSync(logPath, "r");
        fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
        fs.closeSync(fd);
        const content = buf.toString("utf-8");
        const lines = content.split("\n");
        const errorPatterns = [/ERROR/i, /FATAL/i, /PANIC/i, /CRASH/i, /Exception/i, /Traceback/i, /ECONNREFUSED/, /ENOSPC/, /ENOMEM/];
        const warnPatterns = [/WARN/i, /WARNING/i, /deprecated/i];
        for (const line of lines) {
            let isError = false;
            for (const pattern of errorPatterns) {
                if (pattern.test(line)) {
                    isError = true;
                    totalErrors++;
                    const normalized = line.trim()
                        .replace(/\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[.\d]*/g, "")
                        .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, "<id>")
                        .replace(/\b\d{4,}\b/g, "<num>")
                        .trim()
                        .slice(0, 120);
                    const existing = errorCounts.get(normalized);
                    if (existing) {
                        existing.count++;
                        existing.lastSeen = new Date().toISOString();
                    }
                    else {
                        errorCounts.set(normalized, { count: 1, lastSeen: new Date().toISOString() });
                    }
                    break;
                }
            }
            if (!isError) {
                for (const pattern of warnPatterns) {
                    if (pattern.test(line)) {
                        totalWarnings++;
                        break;
                    }
                }
            }
        }
        const topErrors = Array.from(errorCounts.entries())
            .sort((a, b) => b[1].count - a[1].count)
            .slice(0, 3)
            .map(([pattern, data]) => ({ pattern, count: data.count, lastSeen: data.lastSeen }));
        return {
            totalErrors,
            totalWarnings,
            topErrors,
            errorRate: `${totalErrors} errors, ${totalWarnings} warnings in last ${Math.round(readSize / 1024)}KB of logs`,
        };
    }
    catch {
        return { totalErrors: 0, totalWarnings: 0, topErrors: [], errorRate: "failed to read logs" };
    }
}
async function collectOpenClawHealth() {
    const config = readOpenClawConfig();
    const daemonAlive = await checkPort(OPENCLAW_DAEMON_PORT);
    const pid = findOpenClawPid();
    const workspaces = findOpenClawWorkspaces();
    const crons = getOpenClawCrons();
    const version = config?.meta?.lastTouchedVersion || null;
    const probes = await runAllProbes("openclaw", OPENCLAW_DAEMON_PORT, pid, config, crons.map((c) => ({ id: c.id, expression: c.expression, nextRun: c.nextRun })));
    return {
        agentId: "",
        framework: "openclaw",
        version,
        host: os.hostname(),
        timestamp: new Date().toISOString(),
        runtime: { processAlive: pid !== null || daemonAlive, pid, uptimeSeconds: null },
        gateway: { alive: daemonAlive, port: OPENCLAW_DAEMON_PORT, latencyMs: null },
        integrations: getOpenClawIntegrations(config),
        crons,
        memory: getOpenClawMemory(workspaces),
        model: getOpenClawModel(config),
        disk: getOpenClawDisk(workspaces),
        personality: getOpenClawPersonality(workspaces),
        configSnapshot: getSafeConfigSnapshot(config),
        workspace: getOpenClawWorkspaceDetails(workspaces),
        logsTail: getOpenClawLogsTail(),
        sessions: getOpenClawSessionHealth(workspaces),
        logAnalysis: getOpenClawLogAnalysis(),
        probes,
    };
}
// ---------------------------------------------------------------------------
// Hermes health collection
// ---------------------------------------------------------------------------
const HERMES_DIR = path.join(os.homedir(), ".hermes");
const HERMES_CONFIG_PATH = path.join(HERMES_DIR, "config.yaml");
const HERMES_GATEWAY_PORT = 8642;
function readHermesConfig() {
    // Zero-dep: no YAML parser. Try JSON first (some configs are JSON).
    // For YAML, do basic extraction: strip comments and try JSON parse.
    // If that fails, return empty object (hermes YAML is not critical for health).
    try {
        const raw = fs.readFileSync(HERMES_CONFIG_PATH, "utf-8");
        // Try JSON first
        try {
            return JSON.parse(raw);
        }
        catch { }
        // Try stripping YAML comments and parsing as loose JSON
        // Simple YAML -> JSON: won't handle all cases but covers simple flat/nested configs
        try {
            const cleaned = raw
                .replace(/#.*$/gm, "")
                .replace(/\/\/.*$/gm, "")
                .replace(/\/\*[\s\S]*?\*\//g, "");
            return JSON.parse(cleaned);
        }
        catch { }
        // Return empty -- YAML parsing without a dep is unreliable for complex configs
        return {};
    }
    catch {
        return {};
    }
}
function findHermesPid() {
    // 1. Check PID file
    const pidFile = path.join(HERMES_DIR, "gateway.pid");
    try {
        const pid = parseInt(fs.readFileSync(pidFile, "utf-8").trim(), 10);
        if (!isNaN(pid)) {
            try {
                process.kill(pid, 0);
                return pid;
            }
            catch { }
        }
    }
    catch { }
    // 2. Check /tmp/hermes.pid (some versions write here)
    try {
        const pid = parseInt(fs.readFileSync("/tmp/hermes.pid", "utf-8").trim(), 10);
        if (!isNaN(pid)) {
            try {
                process.kill(pid, 0);
                return pid;
            }
            catch { }
        }
    }
    catch { }
    // 3. Fallback: pgrep for hermes process
    try {
        const out = execSync("pgrep -f 'hermes' 2>/dev/null || pgrep -f 'hermes-agent' 2>/dev/null", {
            encoding: "utf-8",
            timeout: 5000,
        }).trim();
        const pids = out.split("\n").map((l) => parseInt(l.trim(), 10)).filter((n) => !isNaN(n) && n !== process.pid);
        if (pids.length > 0)
            return pids[0];
    }
    catch { }
    // 4. Fallback: check pm2 for hermes process
    try {
        const pm2Out = execSync("pm2 jlist 2>/dev/null", { encoding: "utf-8", timeout: 5000 });
        const pm2List = JSON.parse(pm2Out);
        for (const proc of pm2List) {
            const name = (proc.name || "").toLowerCase();
            if ((name.includes("hermes")) && proc.pm2_env?.status === "online") {
                return proc.pid || null;
            }
        }
    }
    catch { }
    return null;
}
function getHermesPersonality() {
    const candidates = [
        path.join(HERMES_DIR, "SOUL.md"),
        path.join(HERMES_DIR, "memory", "SOUL.md"),
    ];
    for (const soulPath of candidates) {
        try {
            const stat = fs.statSync(soulPath);
            const content = fs.readFileSync(soulPath, "utf-8");
            const words = content.split(/\s+/).filter((w) => w.length > 0).length;
            return { file: "SOUL.md", exists: true, lastModified: stat.mtime.toISOString(), wordCount: words };
        }
        catch { }
    }
    return { file: "SOUL.md", exists: false, lastModified: null, wordCount: 0 };
}
function getHermesMemory() {
    let sizeBytes = 0;
    let entryCount = 0;
    let storeReachable = false;
    let lastWrite = null;
    const dbPath = path.join(HERMES_DIR, "state.db");
    try {
        const stat = fs.statSync(dbPath);
        storeReachable = true;
        sizeBytes += stat.size;
        lastWrite = stat.mtime.toISOString();
    }
    catch { }
    const walPath = path.join(HERMES_DIR, "state.db-wal");
    try {
        sizeBytes += fs.statSync(walPath).size;
    }
    catch { }
    const memoryMd = path.join(HERMES_DIR, "memory", "MEMORY.md");
    try {
        const stat = fs.statSync(memoryMd);
        const content = fs.readFileSync(memoryMd, "utf-8");
        entryCount += content.split("\n").filter((l) => l.trim().length > 0).length;
        sizeBytes += stat.size;
        storeReachable = true;
        if (!lastWrite || stat.mtime.toISOString() > lastWrite) {
            lastWrite = stat.mtime.toISOString();
        }
    }
    catch { }
    const userMd = path.join(HERMES_DIR, "memory", "USER.md");
    try {
        const stat = fs.statSync(userMd);
        sizeBytes += stat.size;
        if (!lastWrite || stat.mtime.toISOString() > lastWrite) {
            lastWrite = stat.mtime.toISOString();
        }
    }
    catch { }
    const logsDir = path.join(HERMES_DIR, "logs");
    try {
        const logFiles = fs.readdirSync(logsDir).filter((f) => f.startsWith("session_"));
        entryCount += logFiles.length;
    }
    catch { }
    const memType = storeReachable ? "sqlite" : "none";
    return { type: memType, entryCount, storeReachable, lastWrite, sizeBytes };
}
function getHermesIntegrations(config) {
    const integrations = [];
    const toolsets = config?.platform_toolsets || {};
    const platformNames = Object.keys(toolsets).filter((p) => !["cli"].includes(p));
    for (const platform of platformNames) {
        integrations.push({
            type: platform,
            connected: true,
            tokenExpiresAt: null,
            lastMessageAt: null,
            error: null,
        });
    }
    const channels = config?.channels || {};
    for (const [platform, settings] of Object.entries(channels)) {
        if (!integrations.find((i) => i.type === platform)) {
            integrations.push({
                type: platform,
                connected: !!settings?.enabled,
                tokenExpiresAt: null,
                lastMessageAt: null,
                error: null,
            });
        }
    }
    return integrations;
}
function getHermesCrons(config) {
    const crons = [];
    const cronList = config?.cron || config?.crons || config?.cronjobs || [];
    const jobs = Array.isArray(cronList) ? cronList : [];
    for (const j of jobs) {
        crons.push({
            id: j.id || j.name || j.label || "unnamed",
            expression: j.expression || j.schedule || j.cron || "",
            timezone: j.timezone || null,
            lastRun: j.lastRun || null,
            lastStatus: j.lastStatus || "unknown",
            lastError: j.lastError || null,
            nextRun: j.nextRun || null,
            deliveryTarget: j.to || j.target || null,
        });
    }
    return crons;
}
function getHermesModel(config) {
    const modelConfig = config?.model || {};
    const modelId = modelConfig.default || modelConfig.model || modelConfig.name || "unknown";
    let provider = modelConfig.provider || "auto";
    let apiReachable = false;
    if (provider === "auto") {
        if (process.env.ANTHROPIC_API_KEY || config?.env?.ANTHROPIC_API_KEY) {
            provider = "anthropic";
            apiReachable = true;
        }
        else if (process.env.OPENROUTER_API_KEY || config?.env?.OPENROUTER_API_KEY) {
            provider = "openrouter";
            apiReachable = true;
        }
        else if (process.env.OPENAI_API_KEY || config?.env?.OPENAI_API_KEY) {
            provider = "openai";
            apiReachable = true;
        }
        else if (modelConfig.base_url) {
            provider = "custom";
            apiReachable = true;
        }
    }
    else {
        apiReachable = !!(modelConfig.base_url || process.env.ANTHROPIC_API_KEY || process.env.OPENAI_API_KEY || process.env.OPENROUTER_API_KEY);
    }
    const envFile = path.join(HERMES_DIR, ".env");
    try {
        const envContent = fs.readFileSync(envFile, "utf-8");
        if (envContent.includes("API_KEY") || envContent.includes("api_key")) {
            apiReachable = true;
        }
    }
    catch { }
    return { provider, modelId, apiReachable, authValid: apiReachable, latencyMs: null };
}
function getHermesDisk() {
    const totalBytes = dirSize(HERMES_DIR);
    const sessionsBytes = dirSize(path.join(HERMES_DIR, "sessions"));
    const logsBytes = dirSize(path.join(HERMES_DIR, "logs"));
    return { homeDirMb: bytesToMb(totalBytes), sessionsMb: bytesToMb(sessionsBytes), logsMb: bytesToMb(logsBytes), totalMb: bytesToMb(totalBytes) };
}
function getHermesLogsTail() {
    const gatewayLog = path.join(HERMES_DIR, "logs", "gateway.log");
    try {
        const stat = fs.statSync(gatewayLog);
        const readSize = Math.min(stat.size, 4096);
        const buf = Buffer.alloc(readSize);
        const fd = fs.openSync(gatewayLog, "r");
        fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
        fs.closeSync(fd);
        return buf.toString("utf-8");
    }
    catch { }
    const logsDir = path.join(HERMES_DIR, "logs");
    try {
        const files = fs.readdirSync(logsDir)
            .filter((f) => f.startsWith("session_") && f.endsWith(".json"))
            .map((f) => ({ name: f, mtime: fs.statSync(path.join(logsDir, f)).mtime }))
            .sort((a, b) => b.mtime.getTime() - a.mtime.getTime());
        if (files.length > 0) {
            const logPath = path.join(logsDir, files[0].name);
            const stat = fs.statSync(logPath);
            const readSize = Math.min(stat.size, 4096);
            const buf = Buffer.alloc(readSize);
            const fd = fs.openSync(logPath, "r");
            fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
            fs.closeSync(fd);
            return buf.toString("utf-8");
        }
    }
    catch { }
    return null;
}
function getHermesVersion(config) {
    const hermesAgent = path.join(HERMES_DIR, "hermes-agent");
    try {
        const pkgPath = path.join(hermesAgent, "package.json");
        const pkg = JSON.parse(fs.readFileSync(pkgPath, "utf-8"));
        return pkg.version || null;
    }
    catch { }
    try {
        const pyproject = path.join(hermesAgent, "pyproject.toml");
        const content = fs.readFileSync(pyproject, "utf-8");
        const match = content.match(/version\s*=\s*"([^"]+)"/);
        if (match)
            return match[1];
    }
    catch { }
    return config?.version || null;
}
function getHermesSessionHealth() {
    let totalCount = 0;
    let oldestTimestamp = null;
    let newestTimestamp = null;
    let olderThan30Days = 0;
    let totalSizeBytes = 0;
    const thirtyDaysAgo = Date.now() - 30 * 24 * 60 * 60 * 1000;
    const sessionsDir = path.join(HERMES_DIR, "sessions");
    try {
        const entries = fs.readdirSync(sessionsDir, { withFileTypes: true });
        for (const entry of entries) {
            if (!entry.isFile())
                continue;
            const full = path.join(sessionsDir, entry.name);
            try {
                const stat = fs.statSync(full);
                totalCount++;
                totalSizeBytes += stat.size;
                const mtime = stat.mtime.toISOString();
                if (!oldestTimestamp || mtime < oldestTimestamp)
                    oldestTimestamp = mtime;
                if (!newestTimestamp || mtime > newestTimestamp)
                    newestTimestamp = mtime;
                if (stat.mtime.getTime() < thirtyDaysAgo)
                    olderThan30Days++;
            }
            catch { }
        }
    }
    catch { }
    const logsDir = path.join(HERMES_DIR, "logs");
    try {
        const entries = fs.readdirSync(logsDir).filter((f) => f.startsWith("session_"));
        for (const e of entries) {
            const full = path.join(logsDir, e);
            try {
                const stat = fs.statSync(full);
                totalCount++;
                totalSizeBytes += stat.size;
                const mtime = stat.mtime.toISOString();
                if (!oldestTimestamp || mtime < oldestTimestamp)
                    oldestTimestamp = mtime;
                if (!newestTimestamp || mtime > newestTimestamp)
                    newestTimestamp = mtime;
                if (stat.mtime.getTime() < thirtyDaysAgo)
                    olderThan30Days++;
            }
            catch { }
        }
    }
    catch { }
    return { totalCount, oldestTimestamp, newestTimestamp, olderThan30Days, totalSizeMb: Math.round((totalSizeBytes / (1024 * 1024)) * 10) / 10 };
}
function getHermesLogAnalysis() {
    let totalErrors = 0;
    let totalWarnings = 0;
    const errorCounts = new Map();
    const gatewayLog = path.join(HERMES_DIR, "logs", "gateway.log");
    try {
        const stat = fs.statSync(gatewayLog);
        const readSize = Math.min(stat.size, 32768);
        const buf = Buffer.alloc(readSize);
        const fd = fs.openSync(gatewayLog, "r");
        fs.readSync(fd, buf, 0, readSize, Math.max(0, stat.size - readSize));
        fs.closeSync(fd);
        const content = buf.toString("utf-8");
        const lines = content.split("\n");
        const errorPatterns = [/ERROR/i, /FATAL/i, /PANIC/i, /CRASH/i, /Exception/i, /Traceback/i, /ECONNREFUSED/, /ENOSPC/, /ENOMEM/];
        const warnPatterns = [/WARN/i, /WARNING/i, /deprecated/i];
        for (const line of lines) {
            let isError = false;
            for (const pattern of errorPatterns) {
                if (pattern.test(line)) {
                    isError = true;
                    totalErrors++;
                    const normalized = line.trim()
                        .replace(/\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[.\d]*/g, "")
                        .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, "<id>")
                        .replace(/\b\d{4,}\b/g, "<num>")
                        .trim()
                        .slice(0, 120);
                    const existing = errorCounts.get(normalized);
                    if (existing) {
                        existing.count++;
                        existing.lastSeen = new Date().toISOString();
                    }
                    else {
                        errorCounts.set(normalized, { count: 1, lastSeen: new Date().toISOString() });
                    }
                    break;
                }
            }
            if (!isError) {
                for (const pattern of warnPatterns) {
                    if (pattern.test(line)) {
                        totalWarnings++;
                        break;
                    }
                }
            }
        }
        const topErrors = Array.from(errorCounts.entries())
            .sort((a, b) => b[1].count - a[1].count)
            .slice(0, 3)
            .map(([pattern, data]) => ({ pattern, count: data.count, lastSeen: data.lastSeen }));
        return {
            totalErrors,
            totalWarnings,
            topErrors,
            errorRate: `${totalErrors} errors, ${totalWarnings} warnings in last ${Math.round(readSize / 1024)}KB of logs`,
        };
    }
    catch {
        return { totalErrors: 0, totalWarnings: 0, topErrors: [], errorRate: "no gateway log" };
    }
}
function getHermesSkillValidation(soulContent) {
    const validations = [];
    const soulLower = (soulContent || "").toLowerCase();
    const skillsDir = path.join(HERMES_DIR, "skills");
    try {
        const entries = fs.readdirSync(skillsDir, { withFileTypes: true });
        for (const entry of entries) {
            if (!entry.isDirectory() && !entry.name.endsWith(".md"))
                continue;
            const name = entry.name.replace(/\.md$/, "");
            let hasDefinition = false;
            let isEmpty = true;
            if (entry.isDirectory()) {
                const skillDir = path.join(skillsDir, entry.name);
                try {
                    const skillFiles = fs.readdirSync(skillDir);
                    for (const sf of skillFiles) {
                        if (sf.endsWith(".md") || sf.endsWith(".json") || sf.endsWith(".yaml")) {
                            hasDefinition = true;
                            try {
                                const stat = fs.statSync(path.join(skillDir, sf));
                                if (stat.size > 10)
                                    isEmpty = false;
                            }
                            catch { }
                        }
                    }
                }
                catch { }
            }
            else {
                hasDefinition = true;
                try {
                    const stat = fs.statSync(path.join(skillsDir, entry.name));
                    if (stat.size > 10)
                        isEmpty = false;
                }
                catch { }
            }
            validations.push({ name, hasDefinition, isEmpty: hasDefinition ? isEmpty : true, referencedInSoul: soulLower.includes(name.toLowerCase()) });
        }
    }
    catch { }
    return validations;
}
function getHermesWorkspaceDetails() {
    const files = {};
    const fileMap = {
        "soul": path.join(HERMES_DIR, "SOUL.md"),
        "memory": path.join(HERMES_DIR, "memory", "MEMORY.md"),
        "user": path.join(HERMES_DIR, "memory", "USER.md"),
    };
    for (const [key, filePath] of Object.entries(fileMap)) {
        try {
            const stat = fs.statSync(filePath);
            const content = fs.readFileSync(filePath, "utf-8");
            const words = content.split(/\s+/).filter((w) => w.length > 0).length;
            const lines = content.split("\n").length;
            files[key] = { exists: true, wordCount: words, lineCount: lines, sizeBytes: stat.size, lastModified: stat.mtime.toISOString() };
        }
        catch {
            files[key] = { exists: false, wordCount: 0, lineCount: 0, sizeBytes: 0, lastModified: null };
        }
    }
    const soulContent = files["soul"]?.exists ? (() => {
        try {
            return fs.readFileSync(path.join(HERMES_DIR, "SOUL.md"), "utf-8");
        }
        catch {
            return null;
        }
    })() : null;
    const skillValidation = getHermesSkillValidation(soulContent);
    let skills = { count: 0, names: [], validation: skillValidation };
    const skillsDir = path.join(HERMES_DIR, "skills");
    try {
        const entries = fs.readdirSync(skillsDir, { withFileTypes: true });
        const names = entries
            .filter((e) => e.isDirectory() || e.name.endsWith(".md"))
            .map((e) => e.name);
        skills = { count: names.length, names, validation: skillValidation };
    }
    catch { }
    return { files, skills, pipeline: { exists: false, stageCount: 0 } };
}
async function collectHermesHealth() {
    const config = readHermesConfig();
    const gatewayAlive = await checkPort(HERMES_GATEWAY_PORT);
    const pid = findHermesPid();
    const crons = getHermesCrons(config);
    const probes = await runAllProbes("hermes", HERMES_GATEWAY_PORT, pid, config, crons.map((c) => ({ id: c.id, expression: c.expression, nextRun: c.nextRun })));
    return {
        agentId: "",
        framework: "hermes",
        version: getHermesVersion(config),
        host: os.hostname(),
        timestamp: new Date().toISOString(),
        runtime: { processAlive: pid !== null || gatewayAlive, pid, uptimeSeconds: null },
        gateway: { alive: gatewayAlive, port: HERMES_GATEWAY_PORT, latencyMs: null },
        integrations: getHermesIntegrations(config),
        crons,
        memory: getHermesMemory(),
        model: getHermesModel(config),
        disk: getHermesDisk(),
        personality: getHermesPersonality(),
        configSnapshot: getSafeConfigSnapshot(config),
        workspace: getHermesWorkspaceDetails(),
        logsTail: getHermesLogsTail(),
        sessions: getHermesSessionHealth(),
        logAnalysis: getHermesLogAnalysis(),
        probes,
    };
}
// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------
export async function collectHealth(framework) {
    try {
        if (framework === "hermes") {
            return await collectHermesHealth();
        }
        else if (framework === "openclaw") {
            return await collectOpenClawHealth();
        }
        else {
            throw new Error(`Unknown framework: ${framework}`);
        }
    }
    catch {
        return {
            agentId: "",
            framework,
            version: null,
            host: os.hostname(),
            timestamp: new Date().toISOString(),
            runtime: { processAlive: false, pid: null, uptimeSeconds: null },
            gateway: { alive: false, port: 0, latencyMs: null },
            integrations: [],
            crons: [],
            memory: { type: "none", entryCount: 0, storeReachable: false, lastWrite: null, sizeBytes: 0 },
            model: { provider: "unknown", modelId: "unknown", apiReachable: false, authValid: false, latencyMs: null },
            disk: { homeDirMb: 0, sessionsMb: 0, logsMb: 0, totalMb: 0 },
            personality: { file: "SOUL.md", exists: false, lastModified: null, wordCount: 0 },
            configSnapshot: null,
            workspace: null,
            logsTail: null,
            sessions: { totalCount: 0, oldestTimestamp: null, newestTimestamp: null, olderThan30Days: 0, totalSizeMb: 0 },
            logAnalysis: { totalErrors: 0, totalWarnings: 0, topErrors: [], errorRate: "unknown" },
            probes: [],
        };
    }
}
