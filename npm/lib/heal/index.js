import * as os from "os";
import { detectFramework } from "./detect.js";
import { collectHealth } from "./health.js";
import { collectFileContents } from "./files.js";
import { listRepairActions, getActionCommand, executeRepair, isManualOnly, executeDeferredRepair } from "./repair.js";
import { reasonAboutDiagnosis, reconsiderCounterArguments } from "./reason.js";
import { loadCredentials, registerAndCache, signJWT, } from "./credentials.js";
// ---------------------------------------------------------------------------
// Arg parsing (no commander)
// ---------------------------------------------------------------------------
const DEFAULT_HOSPITAL_URL = "https://api.agent-hospital.ai";
function parseArgs() {
    const args = process.argv.slice(2);
    if (args.length === 0 || args[0] !== "heal") {
        printUsage();
        return null;
    }
    let url = "";
    let framework;
    let json = false;
    for (let i = 1; i < args.length; i++) {
        const arg = args[i];
        if (arg === "--json") {
            json = true;
        }
        else if (arg === "--framework" && i + 1 < args.length) {
            framework = args[++i];
            if (framework !== "hermes" && framework !== "openclaw") {
                console.error(`Invalid framework: ${framework}. Must be "hermes" or "openclaw".`);
                return null;
            }
        }
        else if (!arg.startsWith("-") && !url) {
            url = arg;
        }
    }
    // Default to production hospital
    if (!url) {
        url = DEFAULT_HOSPITAL_URL;
    }
    // Strip trailing slash
    url = url.replace(/\/+$/, "");
    return { url, framework, json };
}
function printUsage() {
    console.error("Usage: hospital-sidecar heal [hospital-url] [--framework openclaw|hermes] [--json]");
    console.error("");
    console.error("Examples:");
    console.error("  npx @agent-hospital/sidecar heal");
    console.error("  npx @agent-hospital/sidecar heal https://my-hospital.example.com");
    console.error("  npx @agent-hospital/sidecar heal --framework openclaw --json");
}
// ---------------------------------------------------------------------------
// HTTP helpers (native fetch, Node 18+)
// ---------------------------------------------------------------------------
async function postJSON(url, body, jwtToken) {
    const headers = {
        "Content-Type": "application/json",
    };
    if (jwtToken) {
        headers["Authorization"] = `Bearer ${jwtToken}`;
    }
    const jsonBody = JSON.stringify(body);
    // Follow redirects manually for POST (some runtimes drop body on redirect)
    let currentUrl = url;
    for (let i = 0; i < 3; i++) {
        const res = await fetch(currentUrl, {
            method: "POST",
            headers,
            body: jsonBody,
            redirect: "manual",
        });
        // Follow 3xx redirects preserving POST + body
        if (res.status >= 300 && res.status < 400) {
            const location = res.headers.get("location");
            if (location) {
                currentUrl = location.startsWith("http") ? location : new URL(location, currentUrl).href;
                continue;
            }
        }
        if (!res.ok) {
            const text = await res.text().catch(() => "");
            throw new Error(`HTTP ${res.status}: ${text.slice(0, 500)}`);
        }
        return res.json();
    }
    throw new Error("Too many redirects");
}
// ---------------------------------------------------------------------------
// Display helpers (no chalk -- plain text)
// ---------------------------------------------------------------------------
function log(msg) {
    console.error(msg);
}
function logDoctor(narrative, severity) {
    const prefix = severity === "critical" ? "[!!!]" : severity === "warning" ? "[ ! ]" : "[ i ]";
    log("");
    log(`${prefix} Doctor: ${narrative}`);
}
function logRepairResults(results) {
    for (const r of results) {
        if (r.success) {
            log(`  [OK]   ${r.action}: ${r.output}`);
        }
        else {
            log(`  [FAIL] ${r.action}: ${r.output}`);
        }
    }
}
function logRecommendations(commands) {
    const manual = commands.filter((c) => !c.whitelisted);
    if (manual.length === 0)
        return;
    log("");
    log("Manual recommendations (run these yourself):");
    for (const cmd of manual) {
        log(`  $ ${cmd.action}`);
        log(`    ${cmd.description}`);
    }
}
// ---------------------------------------------------------------------------
// Post-heal: execute deferred repairs (restart-daemon, kill-port-conflict)
// These are safe to run now that the heal session is complete.
// ---------------------------------------------------------------------------
function runDeferredRepairs(actions, framework, json) {
    if (actions.length === 0) return;
    // Deduplicate
    const unique = [...new Set(actions)];
    if (!json) {
        log("");
        log(`Executing ${unique.length} deferred repair(s) (safe now that heal session is complete):`);
    }
    for (const action of unique) {
        const result = executeDeferredRepair(action, framework);
        if (!json) {
            if (result.success) {
                log(`  [OK]   ${action}: ${result.output}`);
            } else {
                log(`  [FAIL] ${action}: ${result.output}`);
            }
        }
    }
}
// ---------------------------------------------------------------------------
// Main heal flow
// ---------------------------------------------------------------------------
async function main() {
    const parsed = parseArgs();
    if (!parsed) {
        process.exit(1);
    }
    const { url, json } = parsed;
    // Step 1: Detect framework
    const detection = detectFramework(parsed.framework);
    const framework = detection.framework;
    if (!json)
        log(`Detected framework: ${framework}`);
    // Step 2: Collect health data
    if (!json)
        log("Collecting health data...");
    let report;
    try {
        report = await collectHealth(framework);
    }
    catch (err) {
        if (json) {
            console.log(JSON.stringify({ error: "health_collection_failed", message: err.message }));
        }
        else {
            log(`Health collection failed: ${err.message}`);
        }
        process.exit(1);
    }
    // Step 3: Collect file contents
    if (!json)
        log("Collecting workspace files...");
    const fileContents = collectFileContents(framework);
    // Step 4: Get available repair actions
    const availableActions = listRepairActions(framework);
    // Step 5: Resolve credentials (load cached or auto-register)
    let creds;
    const cachedCreds = loadCredentials(url);
    if (cachedCreds) {
        if (!json)
            log("Using cached credentials.");
        creds = cachedCreds;
    }
    else {
        if (!json)
            log("First run: registering with Agent Hospital...");
        try {
            creds = await registerAndCache(url, framework, os.hostname(), json);
            if (!json)
                log("Registered. Credentials saved to ~/.agent-hospital/credentials.json");
        }
        catch (err) {
            if (json) {
                console.log(JSON.stringify({ error: "registration_failed", message: err.message }));
            }
            else {
                log(`Registration failed: ${err.message}`);
            }
            process.exit(1);
        }
    }
    if (!json)
        log("Sending to hospital...");
    // Step 6: Healing loop
    let sessionId;
    let turnCount = 0;
    const MAX_TURNS = 10;
    let lastDisagreements = [];
    let hasReregistered = false;
    const deferredActions = []; // manual-only actions to execute after heal loop
    // Initial request payload
    let requestBody = {
        name: os.hostname(),
        framework,
        host: os.hostname(),
        report,
        fileContents,
        availableActions,
    };
    while (turnCount < MAX_TURNS) {
        turnCount++;
        if (!json) {
            log(turnCount === 1
                ? "Waiting for diagnosis..."
                : `Healing turn ${turnCount}: sending results to doctor...`);
        }
        let decision;
        try {
            // Sign a fresh JWT for this request (60s TTL)
            const jwt = signJWT(creds.fingerprint, creds.privateKeyJwk);
            if (turnCount === 1) {
                const resp = await postJSON(`${url}/api/v1/heal`, requestBody, jwt);
                if (resp.sessionId)
                    sessionId = resp.sessionId;
                decision = resp.decision;
            }
            else {
                const resp = await postJSON(`${url}/api/v1/heal/results`, requestBody, jwt);
                decision = resp.decision;
            }
        }
        catch (err) {
            // Agent was deleted on the server -- re-register once and retry
            if (turnCount === 1 &&
                !hasReregistered &&
                err.message.includes("unknown agent fingerprint")) {
                hasReregistered = true;
                if (!json)
                    log("Agent not recognized (may have been deleted). Re-registering...");
                try {
                    creds = await registerAndCache(url, framework, os.hostname(), json);
                    if (!json)
                        log("Re-registered. Retrying...");
                    turnCount = 0;
                    continue;
                }
                catch {
                    // fall through to normal error handling
                }
            }
            if (json) {
                console.log(JSON.stringify({ error: "server_request_failed", turn: turnCount, message: err.message }));
            }
            else {
                log(`Turn ${turnCount} failed: ${err.message}`);
                if (err.message.includes("ECONNREFUSED") || err.message.includes("fetch failed")) {
                    log(`Cannot reach hospital server at ${url}. Is it running?`);
                }
            }
            process.exit(1);
        }
        // Handle decision
        if (decision.decision === "healed") {
            if (json) {
                console.log(JSON.stringify({
                    decision: "healed",
                    sessionId,
                    narrative: decision.narrative,
                    confidence: decision.confidence,
                    turnsUsed: turnCount,
                }));
            }
            else {
                logDoctor(decision.narrative || "Agent is healthy.");
                log("");
                log(`Healing complete in ${turnCount} turn(s).`);
            }
            runDeferredRepairs(deferredActions, framework, json);
            return;
        }
        if (decision.decision === "escalate") {
            if (json) {
                console.log(JSON.stringify({
                    decision: "escalate",
                    sessionId,
                    narrative: decision.narrative,
                    commands: decision.commands,
                }));
            }
            else {
                logDoctor(decision.narrative || "Escalation required.", "critical");
                const manualCmds = decision.commands?.filter((c) => !c.whitelisted) || [];
                if (manualCmds.length > 0) {
                    log("");
                    log("Recommendations:");
                    for (const cmd of manualCmds) {
                        log(`  $ ${cmd.action}`);
                        log(`    ${cmd.description}`);
                    }
                }
                log("");
                log("Healing failed -- manual intervention required");
            }
            runDeferredRepairs(deferredActions, framework, json);
            process.exit(1);
        }
        if (decision.decision === "more_repairs" || decision.decision === "recheck_health") {
            const commands = decision.commands || [];
            const whitelisted = commands.filter((c) => c.whitelisted);
            const manual = commands.filter((c) => !c.whitelisted);
            if (!json) {
                logDoctor(decision.narrative || "Repairs prescribed.", decision.severity);
            }
            // --- COUNTER-ARGUMENT HANDLING: doctor pushed back on our disagreements ---
            if (decision.counterArguments && decision.counterArguments.length > 0 && lastDisagreements.length > 0) {
                if (!json) {
                    log("");
                    log(`Doctor counter-argued ${decision.counterArguments.length} point(s):`);
                    for (const ca of decision.counterArguments) {
                        log(`  [${ca.insistence.toUpperCase()}] ${ca.finding.slice(0, 60)}`);
                        log(`    Evidence: ${ca.evidence.slice(0, 100)}`);
                    }
                    log("");
                    log("Reconsidering...");
                }
                const soulContent = fileContents?.["SOUL.md"]
                    || fileContents?.["memory/workspace/SOUL.md"]
                    || Object.entries(fileContents || {}).find(([k]) => k.endsWith("SOUL.md"))?.[1]
                    || null;
                const reconsideration = await reconsiderCounterArguments(framework, decision.counterArguments, lastDisagreements, soulContent);
                if (reconsideration && !json) {
                    if (reconsideration.yielded.length > 0) {
                        log(`  Yielded on ${reconsideration.yielded.length} point(s) -- doctor was right`);
                    }
                    if (reconsideration.stillDisputed.length > 0) {
                        log(`  Still disagree on ${reconsideration.stillDisputed.length} point(s) -- escalating those`);
                    }
                    if (reconsideration.reasoning) {
                        log(`  Reasoning: ${reconsideration.reasoning.slice(0, 150)}`);
                    }
                }
                // For mandatory counter-arguments where agent still holds, escalate
                if (reconsideration?.stillDisputed && reconsideration.stillDisputed.length > 0) {
                    const mandatoryHolds = decision.counterArguments.filter(ca => ca.insistence === "mandatory" &&
                        reconsideration.stillDisputed.some(s => s.includes(ca.finding) || ca.finding.includes(s)));
                    if (mandatoryHolds.length > 0 && !json) {
                        log(`  [STANDOFF] ${mandatoryHolds.length} mandatory finding(s) unresolved -- needs human decision`);
                    }
                }
            }
            if (whitelisted.length === 0 && manual.length === 0) {
                // No commands but doctor wants more -- recheck health
                if (!json)
                    log("  Doctor requested fresh health data...");
                const freshReport = await collectHealth(framework);
                requestBody = {
                    sessionId,
                    results: [],
                    postRepairHealth: freshReport,
                };
                continue;
            }
            // --- AGENT REASONING: think about diagnosis before acting ---
            let feedback = null;
            if (commands.length > 0) {
                if (!json)
                    log("");
                if (!json)
                    log("Thinking about diagnosis...");
                // Get SOUL.md content for self-knowledge
                const soulContent = fileContents?.["SOUL.md"]
                    || fileContents?.["memory/workspace/SOUL.md"]
                    || Object.entries(fileContents || {}).find(([k]) => k.endsWith("SOUL.md"))?.[1]
                    || null;
                feedback = await reasonAboutDiagnosis(framework, decision.narrative, commands, soulContent);
                if (feedback && !json) {
                    if (feedback.disagreements.length > 0) {
                        log("");
                        log(`Agent disagrees with ${feedback.disagreements.length} finding(s):`);
                        for (const d of feedback.disagreements) {
                            log(`  [DISAGREE] ${d.action.slice(0, 60)}`);
                            log(`    Reason: ${d.reason}`);
                        }
                    }
                    if (feedback.context) {
                        log(`  Context: ${feedback.context.slice(0, 200)}`);
                    }
                    const agreedCount = feedback.agreements.length;
                    const totalCount = commands.length;
                    log(`  Verdict: agree with ${agreedCount}/${totalCount} findings`);
                }
                if (!feedback && !json) {
                    log("  (no local LLM available -- executing without reasoning)");
                }
                // Save disagreements for counter-argument handling in next turn
                if (feedback) {
                    lastDisagreements = feedback.disagreements;
                }
            }
            // Execute repairs -- only those the agent consents to (or all if no reasoning available)
            const results = [];
            const actionsToExecute = feedback
                ? whitelisted.filter(c => feedback.executeOnly.includes(c.action))
                : whitelisted;
            // Log refused actions
            if (feedback) {
                const refused = whitelisted.filter(c => !feedback.executeOnly.includes(c.action));
                for (const cmd of refused) {
                    results.push({
                        action: cmd.action,
                        success: false,
                        output: `REFUSED by agent: ${feedback.disagreements.find(d => d.action === cmd.action)?.reason || "agent disagreed"}`,
                    });
                    if (!json)
                        log(`  [REFUSED] ${cmd.action}: agent disagreed`);
                }
            }
            if (actionsToExecute.length > 0) {
                if (!json) {
                    log("");
                    log(`Executing ${actionsToExecute.length} agreed repair(s):`);
                }
                for (const cmd of actionsToExecute) {
                    const shellCmd = getActionCommand(cmd.action, framework);
                    if (shellCmd) {
                        const result = executeRepair(cmd.action, framework);
                        results.push({ action: cmd.action, ...result });
                        // Track deferred actions for post-heal execution
                        if (isManualOnly(cmd.action, framework)) {
                            deferredActions.push(cmd.action);
                        }
                    }
                    else {
                        results.push({ action: cmd.action, success: false, output: `Unknown action: ${cmd.action}` });
                    }
                }
                if (!json) {
                    logRepairResults(results);
                    logRecommendations(commands);
                }
            }
            else if (whitelisted.length === 0) {
                // Only manual recommendations -- report as skipped
                if (!json) {
                    logRecommendations(commands);
                }
                for (const cmd of manual) {
                    results.push({
                        action: cmd.action,
                        success: false,
                        output: `Skipped: manual-only action (cannot auto-execute)`,
                    });
                }
            }
            else if (!json) {
                logRecommendations(commands);
            }
            // Collect post-repair health if any repair succeeded
            let postRepairHealth = null;
            if (results.some((r) => r.success)) {
                if (!json)
                    log("Collecting post-repair health data...");
                try {
                    postRepairHealth = await collectHealth(framework);
                }
                catch {
                    if (!json)
                        log("Post-repair health collection failed");
                }
            }
            // Send results + agent feedback back for next turn
            requestBody = {
                sessionId,
                results,
                postRepairHealth,
                ...(feedback ? { agentFeedback: feedback } : {}),
            };
            continue;
        }
        // Unknown decision
        if (json) {
            console.log(JSON.stringify({ error: "unknown_decision", decision: decision.decision }));
        }
        else {
            log(`Unexpected decision from hospital: ${decision.decision}`);
        }
        break;
    }
    if (turnCount >= MAX_TURNS) {
        if (json) {
            console.log(JSON.stringify({ error: "max_turns_reached", turns: MAX_TURNS }));
        }
        else {
            log("");
            log(`Reached maximum healing turns (${MAX_TURNS}). Please investigate manually.`);
        }
        runDeferredRepairs(deferredActions, framework, json);
        process.exit(1);
    }
}
export { main };
