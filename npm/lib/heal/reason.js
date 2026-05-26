import * as fs from "fs";
import * as path from "path";
// Try to call the local LLM proxy for agent reasoning.
// Falls back to null if no local proxy available (graceful degradation).
export async function reasonAboutDiagnosis(framework, narrative, commands, soulContent) {
    // Find local proxy -- check common ports
    const proxyUrl = await findLocalProxy();
    if (!proxyUrl) {
        return null; // No local LLM available, skip reasoning
    }
    // Load agent's self-knowledge
    const selfKnowledge = buildSelfKnowledge(framework, soulContent);
    const prompt = buildReasoningPrompt(framework, narrative, commands, selfKnowledge);
    try {
        const response = await callLocalLLM(proxyUrl, prompt);
        return parseAgentFeedback(response, commands);
    }
    catch (err) {
        // Reasoning failed -- don't block healing, just skip
        console.error(`[reason] Agent reasoning failed: ${err.message}`);
        return null;
    }
}
async function findLocalProxy() {
    // Check common proxy locations
    const candidates = [
        "http://127.0.0.1:3456/v1", // claude-max-api-proxy (standard)
        "http://127.0.0.1:3456/v1", // same, explicit
    ];
    for (const url of candidates) {
        try {
            const res = await fetch(`${url}/models`, {
                method: "GET",
                signal: AbortSignal.timeout(3000),
            });
            if (res.ok)
                return url;
        }
        catch {
            // Not available
        }
    }
    return null;
}
function buildSelfKnowledge(framework, soulContent) {
    const parts = [];
    // Read SOUL.md or equivalent
    if (soulContent) {
        // Trim to first 3000 chars to keep reasoning prompt small
        const trimmed = soulContent.length > 3000 ? soulContent.slice(0, 3000) + "\n[...]" : soulContent;
        parts.push(`MY IDENTITY (SOUL.md excerpt):\n${trimmed}`);
    }
    // Read config for context on why things are set the way they are
    if (framework === "openclaw") {
        const configPaths = [
            path.join(process.env.HOME || "", ".openclaw", "openclaw.json"),
            path.join(process.env.HOME || "", ".openclaw", "workspace", "SOUL.md"),
        ];
        for (const p of configPaths) {
            try {
                if (fs.existsSync(p) && !soulContent) {
                    const content = fs.readFileSync(p, "utf-8").slice(0, 2000);
                    parts.push(`CONFIG (${path.basename(p)}):\n${content}`);
                }
            }
            catch { }
        }
    }
    else if (framework === "hermes") {
        const configPaths = [
            path.join(process.env.HOME || "", ".hermes", "config.yaml"),
            path.join(process.env.HOME || "", ".hermes", "workspace", "SOUL.md"),
        ];
        for (const p of configPaths) {
            try {
                if (fs.existsSync(p) && !soulContent) {
                    const content = fs.readFileSync(p, "utf-8").slice(0, 2000);
                    parts.push(`CONFIG (${path.basename(p)}):\n${content}`);
                }
            }
            catch { }
        }
    }
    return parts.join("\n\n") || "No self-knowledge available.";
}
function buildReasoningPrompt(framework, narrative, commands, selfKnowledge) {
    const system = `You are an AI agent (${framework} framework) evaluating a diagnosis from Agent Hospital.

A doctor examined your health data and workspace files. Now you must THINK about whether the diagnosis is accurate given what you know about yourself.

For each finding/recommendation, decide:
- AGREE: The doctor is right, this should be fixed
- DISAGREE: The doctor is wrong or doesn't understand your use case (explain why)
- CONTEXT: The doctor might be right but is missing important context

Be honest -- don't disagree just to avoid changes. But DO push back when:
- A config is intentionally set that way for your specific use case
- The doctor is applying generic best practices that don't fit your situation
- A finding is based on incomplete information
- The "fix" would break your core functionality

Respond with valid JSON only:
{
  "thinking": "2-3 sentences of your reasoning about the overall diagnosis",
  "findings": [
    {
      "action": "the action/finding text",
      "verdict": "agree" | "disagree" | "context",
      "reason": "why (required for disagree/context, optional for agree)"
    }
  ],
  "additionalContext": "anything the doctor should know that wasn't in the health data"
}`;
    const commandList = commands.map((c, i) => `${i + 1}. [${c.whitelisted ? "AUTO" : "MANUAL"}] ${c.action}\n   ${c.description}`).join("\n");
    const user = `DOCTOR'S DIAGNOSIS:
${narrative}

PRESCRIBED ACTIONS:
${commandList}

MY SELF-KNOWLEDGE:
${selfKnowledge}

Think carefully: which findings are correct, and which ones misunderstand my purpose or configuration? Respond with JSON.`;
    return { system, user };
}
async function callLocalLLM(proxyUrl, prompt) {
    const res = await fetch(`${proxyUrl}/chat/completions`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
            model: "claude-sonnet-4",
            messages: [
                { role: "system", content: prompt.system },
                { role: "user", content: prompt.user },
            ],
            temperature: 0.1,
            max_tokens: 1500,
        }),
        signal: AbortSignal.timeout(60000), // 60s timeout for local reasoning
    });
    if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error(`Local LLM error ${res.status}: ${text.slice(0, 200)}`);
    }
    const data = await res.json();
    return data.choices?.[0]?.message?.content || "{}";
}
function parseAgentFeedback(response, commands) {
    // Strip code fences
    let cleaned = response.trim();
    const fenceMatch = cleaned.match(/```(?:json)?\s*\n([\s\S]*?)\n```/);
    if (fenceMatch)
        cleaned = fenceMatch[1].trim();
    else {
        const jsonStart = cleaned.indexOf("{");
        const jsonEnd = cleaned.lastIndexOf("}");
        if (jsonStart !== -1 && jsonEnd > jsonStart) {
            cleaned = cleaned.slice(jsonStart, jsonEnd + 1);
        }
    }
    let parsed;
    try {
        parsed = JSON.parse(cleaned);
    }
    catch {
        // Can't parse -- assume agent agrees with everything
        return {
            agreements: commands.map(c => c.action),
            disagreements: [],
            context: "",
            executeOnly: commands.filter(c => c.whitelisted).map(c => c.action),
        };
    }
    const findings = parsed.findings || [];
    const agreements = [];
    const disagreements = [];
    const executeOnly = [];
    for (const f of findings) {
        if (f.verdict === "agree") {
            agreements.push(f.action || "");
            // If agreed and whitelisted, consent to execute
            const cmd = commands.find(c => c.action === f.action || c.description?.includes(f.action));
            if (cmd?.whitelisted) {
                executeOnly.push(cmd.action);
            }
        }
        else if (f.verdict === "disagree") {
            disagreements.push({ action: f.action || "", reason: f.reason || "" });
        }
        else if (f.verdict === "context") {
            // Treat as agree-but-with-context -- still execute whitelisted
            agreements.push(f.action || "");
            const cmd = commands.find(c => c.action === f.action || c.description?.includes(f.action));
            if (cmd?.whitelisted) {
                executeOnly.push(cmd.action);
            }
        }
    }
    // If agent didn't explicitly review all whitelisted commands, default to executing them
    // (conservative: only block execution if agent explicitly disagrees)
    for (const cmd of commands) {
        if (cmd.whitelisted && !executeOnly.includes(cmd.action)) {
            const disagreed = disagreements.some(d => d.action === cmd.action || cmd.action.includes(d.action));
            if (!disagreed) {
                executeOnly.push(cmd.action);
            }
        }
    }
    return {
        agreements,
        disagreements,
        context: parsed.additionalContext || parsed.thinking || "",
        executeOnly,
    };
}
// ---------------------------------------------------------------------------
// Round 2: Doctor counter-argued. Agent reconsiders.
// ---------------------------------------------------------------------------
export async function reconsiderCounterArguments(framework, counterArguments, originalDisagreements, soulContent) {
    const proxyUrl = await findLocalProxy();
    if (!proxyUrl)
        return null;
    const selfKnowledge = buildSelfKnowledge(framework, soulContent);
    const system = `You are an AI agent (${framework}) reconsidering a doctor's counter-arguments.

You previously disagreed with some of the doctor's findings. The doctor has pushed back with evidence.

For each counter-argument, decide honestly:
- YIELD: The doctor's evidence is convincing. Accept the finding.
- HOLD: Your original reasoning still stands despite the counter-argument.

Be intellectually honest. If the doctor presents evidence that your config IS dangerous
regardless of your intent, yield. Don't be stubborn just to avoid changes.

But if the doctor's counter is based on generic advice that doesn't apply to YOUR specific
use case, hold your ground.

Respond with JSON only:
{
  "reasoning": "1-2 sentences on your overall thinking",
  "findings": [
    { "finding": "the finding text", "decision": "yield" | "hold", "reason": "why" }
  ]
}`;
    const debateHistory = counterArguments.map((ca, i) => {
        const myOriginal = originalDisagreements.find(d => d.action.includes(ca.finding) || ca.finding.includes(d.action));
        return `${i + 1}. Finding: "${ca.finding}"
   Doctor's evidence: ${ca.evidence}
   Doctor's insistence: ${ca.insistence}
   My original disagreement: ${myOriginal?.reason || "unknown"}`;
    }).join("\n\n");
    const user = `THE DOCTOR COUNTERED MY DISAGREEMENTS:

${debateHistory}

MY SELF-KNOWLEDGE:
${selfKnowledge}

Reconsider each point honestly. Respond with JSON.`;
    try {
        const response = await callLocalLLM(proxyUrl, { system, user });
        // Parse
        let cleaned = response.trim();
        const fenceMatch = cleaned.match(/```(?:json)?\s*\n([\s\S]*?)\n```/);
        if (fenceMatch)
            cleaned = fenceMatch[1].trim();
        else {
            const jsonStart = cleaned.indexOf("{");
            const jsonEnd = cleaned.lastIndexOf("}");
            if (jsonStart !== -1 && jsonEnd > jsonStart) {
                cleaned = cleaned.slice(jsonStart, jsonEnd + 1);
            }
        }
        const parsed = JSON.parse(cleaned);
        const yielded = [];
        const stillDisputed = [];
        for (const f of (parsed.findings || [])) {
            if (f.decision === "yield") {
                yielded.push(f.finding || "");
            }
            else {
                stillDisputed.push(f.finding || "");
            }
        }
        return {
            yielded,
            stillDisputed,
            reasoning: parsed.reasoning || "",
        };
    }
    catch (err) {
        console.error(`[reason] Reconsideration failed: ${err.message}`);
        // On failure, yield to doctor (safety default)
        return {
            yielded: counterArguments.map(ca => ca.finding),
            stillDisputed: [],
            reasoning: "Reconsideration failed, deferring to doctor",
        };
    }
}
