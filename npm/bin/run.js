#!/usr/bin/env node

const args = process.argv.slice(2);
const cmd = args[0];

if (cmd === "heal") {
  // Pure Node.js -- works everywhere, no Go binary needed
  import("../lib/heal/index.js")
    .then((mod) => mod.main())
    .catch((err) => {
      console.error(`Fatal error: ${err.message}`);
      process.exit(1);
    });
} else {
  // Go binary commands: setup, status, uninstall, version, (default=server)
  const { execFileSync } = require("child_process");
  try {
    const { getBinaryPath } = require("../lib/platform");
    const binary = getBinaryPath();
    execFileSync(binary, args, { stdio: "inherit" });
    process.exit(0);
  } catch (err) {
    if (err.status !== undefined) {
      process.exit(err.status);
    }
    if (err.message && err.message.includes("Binary not found")) {
      console.error("Go binary not available (needed for: setup, status, uninstall).");
      console.error("The 'heal' command works without it: npx @agent-hospital/sidecar heal");
    } else {
      console.error(err.message);
    }
    process.exit(1);
  }
}
