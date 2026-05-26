import * as crypto from "crypto";
import * as fs from "fs";
import * as path from "path";
import * as os from "os";
// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------
const CRED_DIR = path.join(os.homedir(), ".agent-hospital");
const CRED_FILE = path.join(CRED_DIR, "credentials.json");
// ---------------------------------------------------------------------------
// Ed25519 keypair generation
// ---------------------------------------------------------------------------
export function generateEd25519Keypair() {
    const kp = crypto.generateKeyPairSync("ed25519");
    // Export raw 32-byte public key: SPKI DER is 44 bytes, last 32 are the key
    const spkiDer = kp.publicKey.export({ type: "spki", format: "der" });
    const rawPublicKey = spkiDer.subarray(12);
    // Export private key as JWK for easy re-import later
    const jwk = kp.privateKey.export({ format: "jwk" });
    return {
        rawPublicKey,
        publicKeyBase64: rawPublicKey.toString("base64"),
        privateKeyJwk: {
            kty: "OKP",
            crv: "Ed25519",
            x: jwk.x,
            d: jwk.d,
        },
    };
}
// ---------------------------------------------------------------------------
// Fingerprint: SHA-256 of raw 32-byte public key
// ---------------------------------------------------------------------------
export function computeFingerprint(rawPublicKey) {
    return crypto.createHash("sha256").update(rawPublicKey).digest("hex");
}
// ---------------------------------------------------------------------------
// JWT signing (Ed25519, zero deps)
// ---------------------------------------------------------------------------
export function signJWT(fingerprint, privateKeyJwk) {
    const header = Buffer.from(JSON.stringify({ alg: "EdDSA", typ: "agent+jwt" })).toString("base64url");
    const iat = Math.floor(Date.now() / 1000);
    const payload = Buffer.from(JSON.stringify({
        sub: fingerprint,
        iat,
        exp: iat + 60,
        jti: crypto.randomUUID(),
    })).toString("base64url");
    const signingInput = `${header}.${payload}`;
    const privKey = crypto.createPrivateKey({
        key: privateKeyJwk,
        format: "jwk",
    });
    const sig = crypto.sign(null, Buffer.from(signingInput), privKey);
    return `${signingInput}.${sig.toString("base64url")}`;
}
// ---------------------------------------------------------------------------
// Credential persistence
// ---------------------------------------------------------------------------
export function loadCredentials(hospitalUrl) {
    try {
        const raw = fs.readFileSync(CRED_FILE, "utf-8");
        const creds = JSON.parse(raw);
        if (creds.hospitalUrl !== hospitalUrl)
            return null;
        if (!creds.fingerprint || !creds.privateKeyJwk?.d)
            return null;
        return creds;
    }
    catch {
        return null;
    }
}
export function saveCredentials(creds) {
    fs.mkdirSync(CRED_DIR, { recursive: true, mode: 0o700 });
    fs.writeFileSync(CRED_FILE, JSON.stringify(creds, null, 2), { mode: 0o600 });
}
// ---------------------------------------------------------------------------
// Registration: host + agent in one shot
// ---------------------------------------------------------------------------
async function httpPost(url, body) {
    const res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
    });
    if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error(`HTTP ${res.status}: ${text.slice(0, 300)}`);
    }
    return res.json();
}
export async function registerAndCache(hospitalUrl, framework, agentName, json) {
    // Step 1: Register host
    const hostResp = await httpPost(`${hospitalUrl}/api/v1/hosts/register`, {
        name: agentName,
    });
    const enrollmentToken = hostResp.enrollmentToken;
    if (!enrollmentToken) {
        throw new Error("Host registration did not return an enrollment token");
    }
    if (!json)
        console.error(`  Host registered.`);
    // Step 2: Generate Ed25519 keypair
    const { rawPublicKey, publicKeyBase64, privateKeyJwk } = generateEd25519Keypair();
    const fingerprint = computeFingerprint(rawPublicKey);
    // Step 3: Register agent
    const agentResp = await httpPost(`${hospitalUrl}/api/v1/agents/register`, {
        hostToken: enrollmentToken,
        publicKey: publicKeyBase64,
        name: agentName,
        framework,
    });
    const agentId = agentResp.agentId;
    if (!agentId) {
        throw new Error("Agent registration did not return an agent ID");
    }
    if (!json)
        console.error(`  Agent registered (${agentId.slice(0, 8)}...).`);
    // Step 4: Cache credentials
    const creds = {
        hospitalUrl,
        agentId,
        fingerprint,
        publicKeyBase64,
        privateKeyJwk,
        registeredAt: new Date().toISOString(),
    };
    saveCredentials(creds);
    return creds;
}
