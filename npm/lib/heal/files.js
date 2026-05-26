import * as fs from "fs";
import * as path from "path";
import * as os from "os";
// ---------------------------------------------------------------------------
// Smart workspace scanner -- collects all useful files for the hospital
// ---------------------------------------------------------------------------
// Dirs to skip entirely during recursive walk
const SKIP_DIRS = new Set([
    ".git", ".next", "node_modules", "briefings", "__pycache__",
    ".cache", "dist", "build",
    "venv", ".venv", "env", "site-packages", "image-cache",
    "reference-projects", ".mypy_cache", ".pytest_cache",
]);
// Dirs that get partial treatment (metadata + recent content, not full scan)
const PARTIAL_DIRS = new Set(["sessions", "completions", "logs"]);
// Extensions to skip (binary / database / lock files)
const SKIP_EXTENSIONS = new Set([
    ".db", ".sqlite", ".pid", ".env", ".db-wal", ".db-shm", ".sock", ".lock",
    ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".svg", ".webp",
    ".mp3", ".mp4", ".wav", ".ogg", ".webm", ".avi", ".mov",
    ".zip", ".tar", ".gz", ".bz2", ".7z", ".rar",
    ".woff", ".woff2", ".ttf", ".otf", ".eot",
    ".exe", ".dll", ".so", ".dylib", ".bin",
    ".pdf", ".doc", ".docx", ".xls", ".xlsx",
]);
// Filenames to skip regardless of location
const SKIP_FILENAMES = new Set([
    ".env", ".env.local", ".env.production", ".env.development",
    "daemon.pid", "gateway.pid", ".DS_Store", "Thumbs.db",
]);
// Hidden dirs we DO allow (everything else starting with . is skipped)
const ALLOWED_HIDDEN_DIRS = new Set([".claude"]);
// Core files get read first (priority ordering)
const CORE_FILES = new Set([
    "SOUL.md", "TOOLS.md", "MEMORY.md", "USER.md", "IDENTITY.md",
    "AGENTS.md", "HEARTBEAT.md", "SKILL.md",
    "openclaw.json", "config.yaml", "jobs.json", "settings.json",
]);
// Limits -- generous because server uses two-phase AI triage (AI picks which files to read)
const MAX_FILE_SIZE = 100 * 1024; // 100KB per file
const MAX_TOTAL_BYTES = 2 * 1024 * 1024; // 2MB total payload (server AI will self-select)
const MAX_FILES = 300;
const PARTIAL_LIST_COUNT = 50; // list 50 most recent in partial dirs
const PARTIAL_CONTENT_COUNT = 5; // send first 2KB of the 5 newest
const PARTIAL_CONTENT_BYTES = 2048;
const LOG_TAIL_LINES = 200;
// Secret redaction patterns
const SECRET_PATTERNS = [
    // JSON: "key": "value" or "token": "value"
    /(["'](?:api[_-]?key|secret[_-]?key|token|password|auth|bearer|access[_-]?token|refresh[_-]?token|private[_-]?key|client[_-]?secret|api[_-]?secret|webhook[_-]?secret)["'])\s*:\s*["']([^"']{8,})["']/gi,
    // YAML: key: value
    /((?:api[_-]?key|secret[_-]?key|token|password|auth|bearer|access[_-]?token|refresh[_-]?token|private[_-]?key|client[_-]?secret|api[_-]?secret|webhook[_-]?secret))\s*:\s*(.{8,})/gi,
    // ENV: KEY=value
    /((?:API[_-]?KEY|SECRET|TOKEN|PASSWORD|AUTH|BEARER|ACCESS[_-]?TOKEN|PRIVATE[_-]?KEY|CLIENT[_-]?SECRET|WEBHOOK[_-]?SECRET))\s*=\s*(.{8,})/gi,
    // Bearer tokens in any context
    /Bearer\s+[A-Za-z0-9\-._~+/]+=*/gi,
    // sk-... style API keys
    /sk-[A-Za-z0-9]{20,}/g,
    // ah_... style keys (agent hospital)
    /ah_[A-Fa-f0-9]{20,}/g,
];
function redactSecrets(content) {
    let result = content;
    for (const pattern of SECRET_PATTERNS) {
        // Reset lastIndex for global regexes
        pattern.lastIndex = 0;
        if (pattern.source.startsWith("(")) {
            // Patterns with capture groups -- keep the key, redact the value
            result = result.replace(pattern, (match, key) => `${key}: "[REDACTED]"`);
        }
        else {
            // Bare patterns (Bearer, sk-, ah_)
            result = result.replace(pattern, "[REDACTED]");
        }
    }
    return result;
}
function isBinary(buffer) {
    // Check first 8KB for null bytes
    const check = buffer.subarray(0, 8192);
    for (let i = 0; i < check.length; i++) {
        if (check[i] === 0)
            return true;
    }
    return false;
}
function readFileSafe(p) {
    try {
        return fs.readFileSync(p, "utf-8");
    }
    catch {
        return null;
    }
}
function statSafe(p) {
    try {
        return fs.statSync(p);
    }
    catch {
        return null;
    }
}
function tailLines(content, lines) {
    const allLines = content.split("\n");
    if (allLines.length <= lines)
        return content;
    return allLines.slice(-lines).join("\n");
}
// Collect entries from a partial-scan directory (sessions, completions, logs)
function collectPartialDir(dirPath, dirRelative, tree, files, stats, currentBytes) {
    let entries;
    try {
        entries = fs.readdirSync(dirPath, { withFileTypes: true });
    }
    catch {
        return;
    }
    // Collect all files with stats
    const fileInfos = [];
    for (const entry of entries) {
        if (!entry.isFile())
            continue;
        const abs = path.join(dirPath, entry.name);
        const rel = path.join(dirRelative, entry.name);
        const st = statSafe(abs);
        if (!st)
            continue;
        fileInfos.push({ name: entry.name, abs, rel, size: st.size, mtimeMs: st.mtimeMs });
    }
    // Sort by mtime descending (newest first)
    fileInfos.sort((a, b) => b.mtimeMs - a.mtimeMs);
    const dirName = path.basename(dirPath);
    const isLogDir = dirName === "logs";
    if (isLogDir) {
        // Logs: send last N lines of each .log file, list recent session logs
        const logFiles = fileInfos.filter(f => f.name.endsWith(".log"));
        const sessionLogs = fileInfos.filter(f => f.name.startsWith("session_") && f.name.endsWith(".json"));
        const otherFiles = fileInfos.filter(f => !f.name.endsWith(".log") && !(f.name.startsWith("session_") && f.name.endsWith(".json")));
        // .log files -- tail content
        for (const lf of logFiles) {
            tree.push({ relativePath: lf.rel, size: lf.size, type: "file" });
            const content = readFileSafe(lf.abs);
            if (content && currentBytes.value < MAX_TOTAL_BYTES) {
                const tailed = tailLines(content, LOG_TAIL_LINES);
                const redacted = redactSecrets(tailed);
                files[lf.rel] = `[last ${LOG_TAIL_LINES} lines of ${lf.name}]\n${redacted}`;
                currentBytes.value += redacted.length;
                stats.filesIncluded++;
            }
        }
        // session_*.json -- list 50 most recent with metadata
        const recentSessionLogs = sessionLogs.slice(0, PARTIAL_LIST_COUNT);
        if (recentSessionLogs.length > 0) {
            const listing = recentSessionLogs.map(f => {
                const date = new Date(f.mtimeMs).toISOString();
                return `  ${f.name}  ${formatSize(f.size)}  ${date}`;
            }).join("\n");
            files[`${dirRelative}/__session_logs_listing__`] = `[${recentSessionLogs.length} most recent session log files]\n${listing}`;
            stats.filesIncluded++;
        }
        for (const sf of recentSessionLogs) {
            tree.push({ relativePath: sf.rel, size: sf.size, type: "file" });
        }
        // Add skipped session logs to tree
        for (const sf of sessionLogs.slice(PARTIAL_LIST_COUNT)) {
            tree.push({ relativePath: sf.rel, size: sf.size, type: "file", skipReason: "older-session-log" });
            stats.filesSkipped++;
            incrementSkip(stats, "older-session-log");
        }
        // Other log files
        for (const of_ of otherFiles) {
            tree.push({ relativePath: of_.rel, size: of_.size, type: "file", skipReason: "log-dir-other" });
        }
    }
    else {
        // sessions/ or completions/ -- list 50 most recent, send first 2KB of 5 newest
        const recent = fileInfos.slice(0, PARTIAL_LIST_COUNT);
        const older = fileInfos.slice(PARTIAL_LIST_COUNT);
        // Build listing
        if (recent.length > 0) {
            const listing = recent.map(f => {
                const date = new Date(f.mtimeMs).toISOString();
                return `  ${f.name}  ${formatSize(f.size)}  ${date}`;
            }).join("\n");
            files[`${dirRelative}/__listing__`] = `[${recent.length} most recent files in ${dirName}/ (${fileInfos.length} total)]\n${listing}`;
            stats.filesIncluded++;
        }
        // Send first 2KB of 5 newest
        const newest = recent.slice(0, PARTIAL_CONTENT_COUNT);
        for (const nf of newest) {
            if (currentBytes.value >= MAX_TOTAL_BYTES)
                break;
            try {
                const buf = Buffer.alloc(PARTIAL_CONTENT_BYTES);
                const fd = fs.openSync(nf.abs, "r");
                const bytesRead = fs.readSync(fd, buf, 0, PARTIAL_CONTENT_BYTES, 0);
                fs.closeSync(fd);
                const preview = redactSecrets(buf.subarray(0, bytesRead).toString("utf-8"));
                const truncNote = nf.size > PARTIAL_CONTENT_BYTES ? `\n[... truncated at 2KB, full file is ${formatSize(nf.size)}]` : "";
                files[nf.rel] = `[preview: first 2KB]\n${preview}${truncNote}`;
                currentBytes.value += preview.length;
                stats.filesIncluded++;
            }
            catch {
                // skip unreadable
            }
        }
        // Add all to tree
        for (const f of recent) {
            tree.push({ relativePath: f.rel, size: f.size, type: "file" });
        }
        for (const f of older) {
            tree.push({ relativePath: f.rel, size: f.size, type: "file", skipReason: "older-than-top-50" });
            stats.filesSkipped++;
            incrementSkip(stats, "older-than-top-50");
        }
    }
}
function formatSize(bytes) {
    if (bytes < 1024)
        return `${bytes}B`;
    if (bytes < 1024 * 1024)
        return `${(bytes / 1024).toFixed(1)}KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)}MB`;
}
function incrementSkip(stats, reason) {
    stats.skipReasons[reason] = (stats.skipReasons[reason] || 0) + 1;
}
// Recursive directory walker -- collects FileEntry for full-content dirs
function walkDir(dirPath, basePath, entries, tree, files, stats, currentBytes) {
    let dirEntries;
    try {
        dirEntries = fs.readdirSync(dirPath, { withFileTypes: true });
    }
    catch {
        return;
    }
    stats.dirsScanned++;
    for (const entry of dirEntries) {
        const abs = path.join(dirPath, entry.name);
        const rel = path.relative(basePath, abs);
        if (entry.isDirectory()) {
            // Check skip rules
            if (SKIP_DIRS.has(entry.name)) {
                tree.push({ relativePath: rel, size: 0, type: "dir", skipReason: "excluded-dir" });
                continue;
            }
            // Hidden dirs: only allow specific ones
            if (entry.name.startsWith(".") && !ALLOWED_HIDDEN_DIRS.has(entry.name)) {
                tree.push({ relativePath: rel, size: 0, type: "dir", skipReason: "hidden-dir" });
                continue;
            }
            // Partial dirs get special handling
            if (PARTIAL_DIRS.has(entry.name)) {
                tree.push({ relativePath: rel, size: 0, type: "dir" });
                collectPartialDir(abs, rel, tree, files, stats, currentBytes);
                continue;
            }
            // Recurse into regular dirs
            tree.push({ relativePath: rel, size: 0, type: "dir" });
            walkDir(abs, basePath, entries, tree, files, stats, currentBytes);
            continue;
        }
        if (!entry.isFile())
            continue;
        const st = statSafe(abs);
        if (!st)
            continue;
        stats.filesFound++;
        tree.push({ relativePath: rel, size: st.size, type: "file" });
        // Check skip conditions
        if (SKIP_FILENAMES.has(entry.name)) {
            stats.filesSkipped++;
            tree[tree.length - 1].skipReason = "excluded-filename";
            incrementSkip(stats, "excluded-filename");
            continue;
        }
        const ext = path.extname(entry.name).toLowerCase();
        if (SKIP_EXTENSIONS.has(ext)) {
            stats.filesSkipped++;
            tree[tree.length - 1].skipReason = "excluded-extension";
            incrementSkip(stats, "excluded-extension");
            continue;
        }
        if (st.size > MAX_FILE_SIZE) {
            stats.filesSkipped++;
            tree[tree.length - 1].skipReason = `too-large (${formatSize(st.size)})`;
            incrementSkip(stats, "too-large");
            continue;
        }
        if (st.size === 0) {
            stats.filesSkipped++;
            tree[tree.length - 1].skipReason = "empty";
            incrementSkip(stats, "empty");
            continue;
        }
        // Queue for reading (priority sort happens later)
        const isCore = CORE_FILES.has(entry.name);
        entries.push({ relativePath: rel, absolutePath: abs, size: st.size, mtimeMs: st.mtimeMs, isCore });
    }
}
export function collectFileContents(framework) {
    const files = {};
    const homeDir = os.homedir();
    const tree = [];
    const stats = {
        filesFound: 0,
        filesIncluded: 0,
        filesSkipped: 0,
        totalBytes: 0,
        skipReasons: {},
        dirsScanned: 0,
    };
    const currentBytes = { value: 0 };
    // Determine root paths to scan based on framework
    const scanRoots = [];
    if (framework === "openclaw") {
        const baseDir = path.join(homeDir, ".openclaw");
        // Config file (top-level)
        const configPath = path.join(baseDir, "openclaw.json");
        if (fs.existsSync(configPath)) {
            scanRoots.push({ path: configPath, label: "openclaw.json" });
        }
        // Cron jobs
        const cronPath = path.join(baseDir, "cron", "jobs.json");
        if (fs.existsSync(cronPath)) {
            scanRoots.push({ path: cronPath, label: "cron/jobs.json" });
        }
        // All workspace-* directories
        try {
            const entries = fs.readdirSync(baseDir);
            for (const e of entries) {
                if (e.startsWith("workspace-") || e === "workspace") {
                    const wsPath = path.join(baseDir, e);
                    if (fs.statSync(wsPath).isDirectory()) {
                        scanRoots.push({ path: wsPath, label: e });
                    }
                }
            }
        }
        catch { }
        // Global memory directory
        const memDir = path.join(baseDir, "memory");
        if (fs.existsSync(memDir)) {
            scanRoots.push({ path: memDir, label: "memory" });
        }
        // Completions directory (partial scan)
        const compDir = path.join(baseDir, "completions");
        if (fs.existsSync(compDir)) {
            scanRoots.push({ path: compDir, label: "completions" });
        }
        // Logs directory (partial scan)
        const logsDir = path.join(baseDir, "logs");
        if (fs.existsSync(logsDir)) {
            scanRoots.push({ path: logsDir, label: "logs" });
        }
    }
    else {
        // Hermes: scan entire ~/.hermes/
        const hermesDir = path.join(homeDir, ".hermes");
        if (fs.existsSync(hermesDir)) {
            scanRoots.push({ path: hermesDir, label: ".hermes" });
        }
    }
    // Phase 1: Walk all roots, collecting file entries and building tree
    const fileEntries = [];
    for (const root of scanRoots) {
        const st = statSafe(root.path);
        if (!st)
            continue;
        if (st.isFile()) {
            // Single file (e.g., openclaw.json, jobs.json)
            stats.filesFound++;
            tree.push({ relativePath: root.label, size: st.size, type: "file" });
            fileEntries.push({
                relativePath: root.label,
                absolutePath: root.path,
                size: st.size,
                mtimeMs: st.mtimeMs,
                isCore: CORE_FILES.has(path.basename(root.path)),
            });
        }
        else if (st.isDirectory()) {
            tree.push({ relativePath: root.label, size: 0, type: "dir" });
            // If this IS a partial dir at root level (completions/, logs/)
            if (PARTIAL_DIRS.has(path.basename(root.path))) {
                collectPartialDir(root.path, root.label, tree, files, stats, currentBytes);
            }
            else {
                walkDir(root.path, root.path, fileEntries, tree, files, stats, currentBytes);
                // Fix relative paths: prefix with root label
                for (const fe of fileEntries) {
                    if (!fe.relativePath.startsWith(root.label)) {
                        // Only prefix entries that came from this walk
                        const absCheck = path.resolve(root.path, fe.relativePath);
                        if (absCheck.startsWith(root.path)) {
                            fe.relativePath = path.join(root.label, fe.relativePath);
                        }
                    }
                }
            }
        }
    }
    // Phase 2: Priority sort and read files
    // Core files first, then smaller files first
    fileEntries.sort((a, b) => {
        if (a.isCore && !b.isCore)
            return -1;
        if (!a.isCore && b.isCore)
            return 1;
        return a.size - b.size;
    });
    let filesRead = 0;
    for (const entry of fileEntries) {
        if (filesRead >= MAX_FILES) {
            stats.filesSkipped++;
            incrementSkip(stats, "max-files-reached");
            continue;
        }
        if (currentBytes.value >= MAX_TOTAL_BYTES) {
            stats.filesSkipped++;
            incrementSkip(stats, "payload-cap-reached");
            continue;
        }
        try {
            const buf = fs.readFileSync(entry.absolutePath);
            if (isBinary(buf)) {
                stats.filesSkipped++;
                incrementSkip(stats, "binary-content");
                // Update tree entry with skip reason
                const treeIdx = tree.findIndex(t => t.relativePath === entry.relativePath);
                if (treeIdx >= 0)
                    tree[treeIdx].skipReason = "binary-content";
                continue;
            }
            const content = redactSecrets(buf.toString("utf-8"));
            files[entry.relativePath] = content;
            currentBytes.value += content.length;
            stats.filesIncluded++;
            stats.totalBytes += content.length;
            filesRead++;
        }
        catch {
            stats.filesSkipped++;
            incrementSkip(stats, "read-error");
        }
    }
    // Phase 3: Build directory tree string
    const treeLines = [];
    for (const t of tree) {
        const prefix = t.type === "dir" ? "[dir] " : "      ";
        const sizeStr = t.type === "file" ? ` (${formatSize(t.size)})` : "";
        const skipStr = t.skipReason ? ` [SKIPPED: ${t.skipReason}]` : "";
        treeLines.push(`${prefix}${t.relativePath}${sizeStr}${skipStr}`);
    }
    files["__directory_tree__"] = treeLines.join("\n");
    // Phase 4: Build scan stats
    files["__scan_stats__"] = JSON.stringify({
        filesFound: stats.filesFound,
        filesIncluded: stats.filesIncluded,
        filesSkipped: stats.filesSkipped,
        totalBytes: stats.totalBytes,
        dirsScanned: stats.dirsScanned,
        skipReasons: stats.skipReasons,
        payloadCapReached: currentBytes.value >= MAX_TOTAL_BYTES,
        maxFilesReached: filesRead >= MAX_FILES,
    });
    return files;
}
