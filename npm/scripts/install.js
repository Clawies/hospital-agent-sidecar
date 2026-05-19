const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");

const { getBinaryName, PLATFORM_MAP } = require("../lib/platform");
const { version } = require("../package.json");

const GITHUB_REPO = "Clawies/hospital-agent-sidecar";

function download(url) {
  return new Promise((resolve, reject) => {
    const client = url.startsWith("https") ? https : http;
    client
      .get(url, { headers: { "User-Agent": "hospital-sidecar-npm" } }, (res) => {
        // Follow redirects (GitHub releases redirect to S3)
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          return download(res.headers.location).then(resolve, reject);
        }
        if (res.statusCode === 404) {
          reject(
            new Error(
              `Release v${version} not found on GitHub.\n` +
                `Check: https://github.com/${GITHUB_REPO}/releases`
            )
          );
          return;
        }
        if (res.statusCode !== 200) {
          reject(new Error(`HTTP ${res.statusCode} from ${url}`));
          return;
        }
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () => resolve(Buffer.concat(chunks)));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

async function install() {
  let binaryName;
  try {
    binaryName = getBinaryName();
  } catch (err) {
    // Unsupported platform -- don't fail the install, just warn
    console.warn(`WARNING: ${err.message}`);
    console.warn("hospital-sidecar will not work on this platform.");
    return;
  }

  const binDir = path.join(__dirname, "..", "bin");
  const binaryPath = path.join(binDir, binaryName);

  // Skip if binary already exists (local development or cached)
  if (fs.existsSync(binaryPath)) {
    const stat = fs.statSync(binaryPath);
    if (stat.size > 1000) {
      // Looks like a real binary, not a placeholder
      console.log(`hospital-sidecar binary already present (${(stat.size / 1024 / 1024).toFixed(1)}MB)`);
      return;
    }
  }

  const url = `https://github.com/${GITHUB_REPO}/releases/download/v${version}/${binaryName}`;
  console.log(`Downloading hospital-sidecar v${version} for ${process.platform}/${process.arch}...`);
  console.log(`  ${url}`);

  const data = await download(url);

  if (!fs.existsSync(binDir)) {
    fs.mkdirSync(binDir, { recursive: true });
  }

  fs.writeFileSync(binaryPath, data, { mode: 0o755 });
  console.log(`hospital-sidecar installed (${(data.length / 1024 / 1024).toFixed(1)}MB)`);
}

install().catch((err) => {
  console.error(`Failed to install hospital-sidecar: ${err.message}`);
  console.error("");
  console.error("You can manually download the binary from:");
  console.error(`  https://github.com/${GITHUB_REPO}/releases`);
  console.error("");
  console.error("Then place it at:");
  console.error(`  ${path.join(__dirname, "..", "bin", getBinaryName())}`);
  process.exit(1);
});
