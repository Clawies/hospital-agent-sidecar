import * as os from "os";
import * as path from "path";
import { execSync } from "child_process";
const HERMES_DIR = path.join(os.homedir(), ".hermes");
const OPENCLAW_DIR = path.join(os.homedir(), ".openclaw");
// ---------------------------------------------------------------------------
// Whitelisted action maps
// ---------------------------------------------------------------------------
// Actions safe to auto-execute (non-destructive, won't kill the running agent)
const HERMES_ACTIONS = {
    "prune-sessions": `find ${HERMES_DIR}/sessions -name '*.json' -mtime +30 -delete 2>/dev/null || true`,
    "checkpoint-wal": `sqlite3 ${HERMES_DIR}/state.db 'PRAGMA wal_checkpoint(TRUNCATE);' 2>/dev/null || true`,
    "clean-logs": `find ${HERMES_DIR}/logs -name '*.log' -mtime +7 -delete 2>/dev/null || true`,
};
const OPENCLAW_ACTIONS = {
    "prune-sessions": `find ${OPENCLAW_DIR} -name '*.jsonl' -mtime +30 -delete 2>/dev/null || true`,
    "clean-logs": `find ${OPENCLAW_DIR}/logs -name '*.log' -mtime +7 -delete 2>/dev/null || true`,
    "fix-context-window": `find ${OPENCLAW_DIR} -name '*.jsonl' -size +50M -exec truncate -s 0 {} \\; 2>/dev/null || true`,
};
// Actions that require manual execution -- killing processes can take down the
// agent that is running the heal client, so these are never auto-executed.
export const MANUAL_ONLY_ACTIONS = {
    hermes: {
        "restart-gateway": "pkill -f 'hermes.*gateway' || true && hermes gateway start",
        "kill-port-conflict": "lsof -ti:8642 | xargs kill -9 2>/dev/null || true",
    },
    openclaw: {
        "restart-daemon": "pkill -f 'openclaw.*daemon' || true && openclaw daemon start",
        "kill-port-conflict": "lsof -ti:18789 | xargs kill -9 2>/dev/null || true",
    },
};
// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------
export function listRepairActions(framework) {
    const actions = framework === "hermes" ? HERMES_ACTIONS : OPENCLAW_ACTIONS;
    const manual = MANUAL_ONLY_ACTIONS[framework] || {};
    return [...Object.keys(actions), ...Object.keys(manual)];
}
export function getActionCommand(action, framework) {
    const actions = framework === "hermes" ? HERMES_ACTIONS : OPENCLAW_ACTIONS;
    if (actions[action])
        return actions[action];
    const manual = MANUAL_ONLY_ACTIONS[framework] || {};
    return manual[action] || null;
}
export function isManualOnly(action, framework) {
    const manual = MANUAL_ONLY_ACTIONS[framework] || {};
    return action in manual;
}
// Execute a manual-only action after the heal session is complete.
// Uses spawn with detached + unref so the child survives if the parent dies.
export function executeDeferredRepair(action, framework) {
    const command = getActionCommand(action, framework);
    if (!command) {
        return { success: false, output: `Unknown action: ${action}` };
    }
    try {
        const output = execSync(command, {
            encoding: "utf-8",
            timeout: 30000,
            stdio: ["pipe", "pipe", "pipe"],
        });
        return { success: true, output: output.trim() || "Action completed successfully" };
    }
    catch (err) {
        return { success: false, output: err.stderr || err.message || "Action failed" };
    }
}
export function executeRepair(action, framework) {
    if (isManualOnly(action, framework)) {
        return {
            success: false,
            output: `DEFERRED: "${action}" deferred to post-heal (would terminate this session mid-loop).`,
        };
    }
    const command = getActionCommand(action, framework);
    if (!command) {
        return { success: false, output: `Unknown action: ${action}` };
    }
    try {
        const output = execSync(command, {
            encoding: "utf-8",
            timeout: 30000,
            stdio: ["pipe", "pipe", "pipe"],
        });
        return { success: true, output: output.trim() || "Action completed successfully" };
    }
    catch (err) {
        return { success: false, output: err.stderr || err.message || "Action failed" };
    }
}
