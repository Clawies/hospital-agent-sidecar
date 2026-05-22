#!/usr/bin/env node

const { execFileSync } = require("child_process");
const { getBinaryPath } = require("../lib/platform");

try {
  const binary = getBinaryPath();
  const result = execFileSync(binary, process.argv.slice(2), {
    stdio: "inherit",
  });
  process.exit(0);
} catch (err) {
  if (err.status !== undefined) {
    // execFileSync threw because the child exited with non-zero
    process.exit(err.status);
  }
  console.error(err.message);
  process.exit(1);
}
