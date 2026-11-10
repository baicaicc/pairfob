// This fork's own deployment files (docs/kidsbox.md): the pair.kidsbox.cc
// Worker config must follow upstream's production config, and the deploy
// script must stop before a D1 migration nobody approved.
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";

const root = new URL("..", import.meta.url).pathname;

function jsonc(path: string): Record<string, unknown> {
  const text = readFileSync(join(root, path), "utf8").split("\n").filter(line => !/^\s*\/\//.test(line)).join("\n");
  return JSON.parse(text) as Record<string, unknown>;
}

describe("wrangler.kidsbox.jsonc", () => {
  const upstream = jsonc("workers/pairfob-origin/wrangler.jsonc");
  const fork = jsonc("workers/pairfob-origin/wrangler.kidsbox.jsonc");

  test("carries every upstream setting except the analytics binding", () => {
    const missing = Object.keys(upstream).filter(key => !(key in fork) && key !== "analytics_engine_datasets");
    expect(missing).toEqual([]);
    expect("analytics_engine_datasets" in fork).toBe(false);
  });

  test("matches upstream runtime, bindings and assets", () => {
    for (const key of ["name", "main", "compatibility_date", "compatibility_flags", "workers_dev", "preview_urls", "observability", "durable_objects", "migrations", "assets"]) {
      expect({ key, value: fork[key] }).toEqual({ key, value: upstream[key] });
    }
    const vars = (config: Record<string, unknown>) => config.vars as Record<string, string>;
    expect(Object.keys(vars(fork)).sort()).toEqual(Object.keys(vars(upstream)).sort());
    expect(vars(fork).P2P_OPEN).toBe(vars(upstream).P2P_OPEN);
    // The native shells are built for pairfob.com; no web-view origin is trusted here.
    expect(vars(fork).APP_ORIGINS).toBe("");
    const [forkDB] = fork.d1_databases as Array<Record<string, string>>;
    const [upstreamDB] = upstream.d1_databases as Array<Record<string, string>>;
    expect({ ...forkDB, database_id: "" }).toEqual({ ...upstreamDB, database_id: "" });
    expect(forkDB.database_id).not.toBe(upstreamDB.database_id);
    expect(fork.routes).toEqual([{ pattern: "pair.kidsbox.cc", custom_domain: true }]);
  });
});

describe("deploy-kidsbox.sh", () => {
  test("stops at pending D1 migrations before building or deploying", () => {
    const bin = mkdtempSync(join(tmpdir(), "pf-kidsbox-"));
    try {
      const log = join(bin, "calls.log");
      for (const tool of ["bunx", "bun", "curl"]) {
        writeFileSync(join(bin, tool), `#!/usr/bin/env bash
echo "${tool} $*" >> "${log}"
if [[ "${tool}" == bunx && "$*" == *"migrations list"* ]]; then
  printf 'Migrations to be applied:\\n│ 0007_conversion.sql │\\n│ 0008_next.sql │\\n'
fi
`);
        chmodSync(join(bin, tool), 0o755);
      }
      const run = Bun.spawnSync(["bash", join(root, "scripts/deploy-kidsbox.sh")], {
        env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, PAIRFOB_APPLY_MIGRATIONS: "" },
        stdout: "pipe", stderr: "pipe",
      });
      expect(run.exitCode).toBe(3);
      expect(run.stdout.toString()).toContain("0007_conversion.sql");
      expect(run.stdout.toString()).toContain("0008_next.sql");
      expect(run.stderr.toString()).toContain("PAIRFOB_APPLY_MIGRATIONS=1");
      const calls = readFileSync(log, "utf8").trim().split("\n");
      expect(calls).toHaveLength(1);
      expect(calls[0]).toContain("wrangler d1 migrations list pairfob --remote --config wrangler.kidsbox.jsonc");
    } finally {
      rmSync(bin, { recursive: true, force: true });
    }
  });
});

describe("build-kidsbox-daemon.sh", () => {
  test("stamps a version the update channels never order", () => {
    const script = readFileSync(join(root, "scripts/build-kidsbox-daemon.sh"), "utf8");
    expect(script).toContain('VERSION="kidsbox-$(date -u +%Y%m%d)-${sha}"');
    expect(script).not.toContain("PublicKeyHex");
    // The daemon's and the PWA's release pattern (update_remote.go, lib/daemon-version.ts).
    const release = /^v?\d+(?:[.-]\d+){1,3}$/;
    for (const version of ["kidsbox-20261109-abc1234", "kidsbox-20261109-1234567", "kidsbox-20261109-abc1234-dirty"]) {
      expect(release.test(version)).toBe(false);
    }
  });
});
