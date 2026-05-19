const path = require("path");
const fs = require("fs");

const PLATFORM_MAP = {
  "linux-x64": "hospital-sidecar-linux-amd64",
  "linux-arm64": "hospital-sidecar-linux-arm64",
  "darwin-x64": "hospital-sidecar-darwin-amd64",
  "darwin-arm64": "hospital-sidecar-darwin-arm64",
};

function getBinaryName() {
  const key = `${process.platform}-${process.arch}`;
  const name = PLATFORM_MAP[key];
  if (!name) {
    throw new Error(
      `Unsupported platform: ${process.platform} ${process.arch}\n` +
        `Supported: ${Object.keys(PLATFORM_MAP).join(", ")}`
    );
  }
  return name;
}

function getBinaryPath() {
  const name = getBinaryName();
  const binDir = path.join(__dirname, "..", "bin");
  const binaryPath = path.join(binDir, name);
  if (fs.existsSync(binaryPath)) {
    return binaryPath;
  }
  throw new Error(
    `Binary not found: ${binaryPath}\n` +
      "Try reinstalling: npm install -g @agent-hospital/sidecar"
  );
}

module.exports = { getBinaryName, getBinaryPath, PLATFORM_MAP };
