import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { mkdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { test as base, expect } from "@playwright/test";

const e2eDir = __dirname;
export const repoRoot = path.resolve(e2eDir, "..");
const artifactsDir = path.join(e2eDir, ".artifacts");
const binaryPath = path.join(artifactsDir, `sshmgr${process.platform === "win32" ? ".exe" : ""}`);

// buildBinary 在整轮浏览器测试开始前编译一次 sshmgr，测试进程全部用同一
// 个二进制，避免每个用例重复编译。
export async function buildBinary(): Promise<void> {
  await mkdir(artifactsDir, { recursive: true });
  await new Promise<void>((resolve, reject) => {
    const build = spawn("go", ["build", "-o", binaryPath, "./cmd/sshmgr"], {
      cwd: repoRoot,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let output = "";
    build.stdout.on("data", (chunk) => (output += chunk));
    build.stderr.on("data", (chunk) => (output += chunk));
    build.on("error", reject);
    build.on("close", (code) => (code === 0 ? resolve() : reject(new Error(`go build 失败：${output}`))));
  });
}

// standaloneUI 描述一次已启动的单机 Web UI 进程。
export type standaloneUI = {
  startupURL: string;
  backupDir: string;
  passphraseFile: string;
  close: () => Promise<void>;
};

// clientUI 描述一次已启动的客户端 Web UI 进程。临时目录留给操作系统清理，
// 测试助手不做递归删除。
export type clientUI = {
  startupURL: string;
  close: () => Promise<void>;
};

// startStandaloneUI 为当前用例准备一个全新的临时加密库，以单机角色启动
// `sshmgr ui`，并返回一次性启动链接。子进程的 PATH 指向不存在的目录，
// 这样「自动打开系统浏览器」会失败并只打印链接，由 Playwright 自己使用
// 这条一次性链接，避免真实浏览器抢先消耗它。
async function startStandaloneUI(setup?: (runDir: string) => void): Promise<standaloneUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-"));
  setup?.(runDir);
  const seed = spawnSync("go", ["run", "./e2e/tools/seedvault", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
    },
  });
  if (seed.status !== 0) {
    await rm(runDir, { recursive: true, force: true });
    throw new Error(`准备临时加密库失败：${seed.stdout}\n${seed.stderr}`);
  }
  const backupDir = path.join(runDir, "backups");
  const passFile = path.join(runDir, "backup.pass");

  const ui = spawn(binaryPath, ["ui"], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
      PATH: path.join(runDir, "empty-path"),
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));

  const startupURL = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待 Web UI 启动超时，输出：\n${output}`)),
      15_000,
    );
    const poll = setInterval(() => {
      const match = output.match(/https?:\/\/\S+/);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    ui.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`Web UI 进程提前退出（${code}），输出：\n${output}`));
    });
  });

  return {
    startupURL,
    backupDir,
    passphraseFile: passFile,
    close: async () => {
      ui.kill("SIGTERM");
      await new Promise<void>((resolve) => {
        if (ui.exitCode !== null) {
          resolve();
          return;
        }
        ui.on("exit", () => resolve());
      });
      await rm(runDir, { recursive: true, force: true });
    },
  };
}

// startClientUI 为当前用例生成一个客户端本地快照缓存，并以客户端角色启
// 动 `sshmgr ui`。PATH 指向不存在目录，避免系统浏览器抢先消耗启动链接。
async function startClientUI(): Promise<clientUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-client-"));
  const seed = spawnSync("go", ["run", "./e2e/tools/seedclient", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
    },
  });
  if (seed.status !== 0) {
    throw new Error(`准备客户端缓存失败：${seed.stdout}\n${seed.stderr}`);
  }

  const ui = spawn(binaryPath, ["ui"], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_CACHE_DIR: path.join(runDir, "cache"),
      SSHMGR_CACHE_DEK: path.join(runDir, "cache-dek.key"),
      PATH: path.join(runDir, "empty-path"),
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));

  const startupURL = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待客户端 Web UI 启动超时，输出：\n${output}`)),
      15_000,
    );
    const poll = setInterval(() => {
      const match = output.match(/https?:\/\/\S+/);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    ui.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`客户端 Web UI 进程提前退出（${code}），输出：\n${output}`));
    });
  });

  return {
    startupURL,
    close: async () => {
      ui.kill("SIGTERM");
      await new Promise<void>((resolve) => {
        if (ui.exitCode !== null) {
          resolve();
          return;
        }
        ui.on("exit", () => resolve());
      });
    },
  };
}

// standaloneUIFixture 让每个用例拿到自己的 Web UI 进程；用例结束（无论成
// 败）都会停止进程并删除临时加密库。
type standaloneUIFixture = { standaloneUI: standaloneUI };
type clientUIFixture = { clientUI: clientUI };

export const test = base.extend<standaloneUIFixture & clientUIFixture>({
  standaloneUI: async ({}, use) => {
    const ui = await startStandaloneUI();
    await use(ui);
    await ui.close();
  },
  clientUI: async ({}, use) => {
    const ui = await startClientUI();
    await use(ui);
    await ui.close();
  },
});

export { expect, startClientUI, startStandaloneUI };

// openStandaloneUI 用一次性启动链接进入 Web UI，并等待服务器列表出现。
export async function openStandaloneUI(
  page: import("@playwright/test").Page,
  ui: standaloneUI,
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "服务器" }).waitFor();
}

// openClientUI 用一次性启动链接进入客户端 Web UI，并等待服务器列表出现。
export async function openClientUI(
  page: import("@playwright/test").Page,
  ui: clientUI,
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "服务器" }).waitFor();
}
