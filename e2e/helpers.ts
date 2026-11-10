import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { mkdir, rm } from "node:fs/promises";
import * as https from "node:https";
import * as net from "node:net";
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

// authorityUI 描述一次已启动的权威端 Web UI 进程。浏览器测试用临时加密库
// 与随机端口运行，不接触本机真实权威端数据。
export type authorityUI = {
  loginURL: string;
  password: string;
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

function reserveLocalPort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const listener = net.createServer();
    listener.once("error", reject);
    listener.listen(0, "127.0.0.1", () => {
      const address = listener.address();
      if (typeof address !== "object" || address === null) {
        listener.close(() => reject(new Error("无法取得临时监听端口")));
        return;
      }
      const port = address.port;
      listener.close(() => resolve(port));
    });
  });
}

// startAuthorityUI 生成角色为权威端的临时加密库，设置管理员口令与随机
// Web UI 端口，再启动 serve。发现服务显式关闭，测试不占用本机的 UDP 7878。
async function startAuthorityUI(password: string): Promise<authorityUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-authority-"));
  const servePort = await reserveLocalPort();
  const webPort = await reserveLocalPort();
  const seed = spawnSync("go", ["run", "./e2e/tools/seedvault", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      SSHMGR_E2E_ROLE: "server",
      SSHMGR_E2E_WEB_PORT: String(webPort),
      SSHMGR_E2E_ADMIN_PASSWORD: password,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
    },
  });
  if (seed.status !== 0) {
    throw new Error(`准备权威端临时加密库失败：${seed.stdout}\n${seed.stderr}`);
  }
  const ui = spawn(binaryPath, [
    "serve", "--addr", `127.0.0.1:${servePort}`, "--discovery=false",
    "--tls-cert", path.join(runDir, "browser-test-cert.pem"),
    "--tls-key", path.join(runDir, "browser-test-key.pem"),
  ], {
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
      SSHMGR_E2E_REVERIFY_WINDOW: "100ms",
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));
  const loginURL = `https://127.0.0.1:${webPort}/ui/`;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待权威端 Web UI 超时，输出：\n${output}`)),
      20_000,
    );
    const tryConnect = () => {
      const request = https.get(loginURL, { rejectUnauthorized: false }, (response) => {
        response.resume();
        clearTimeout(timer);
        request.destroy();
        resolve();
      });
      request.once("error", () => request.destroy());
    };
    const poll = setInterval(tryConnect, 100);
    ui.once("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`权威端 Web UI 提前退出（${code}），输出：\n${output}`));
    });
    tryConnect();
  });
  return {
    loginURL,
    password,
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
type authorityUIFixture = { authorityUI: authorityUI };

export const test = base.extend<standaloneUIFixture & clientUIFixture & authorityUIFixture>({
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
  authorityUI: async ({}, use) => {
    const ui = await startAuthorityUI("browser-test-password");
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

// openAuthorityUI 进入权威端登录页并完成管理员口令登录。
export async function openAuthorityUI(
  page: import("@playwright/test").Page,
  ui: authorityUI,
  password: string,
): Promise<void> {
  await page.goto(ui.loginURL);
  await page.getByLabel("管理员口令").fill(password);
  await page.getByRole("button", { name: "登录" }).click();
  await page.getByRole("heading", { name: "服务器" }).waitFor();
}
